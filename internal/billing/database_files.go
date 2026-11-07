package billing

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// DatabaseFileCheckInterval is how often host calls check the database's
// write-ahead log files. The check runs inside usage.handle and management
// calls, so a quiet plugin checks again on its next call.
const DatabaseFileCheckInterval = time.Minute

// WALFileReport says whether a database's -wal and -shm files are still the
// ones it opened with. Checked is false when the database cannot tell.
type WALFileReport struct {
	Checked  bool
	Missing  []string
	Replaced []string
}

func (r WALFileReport) Healthy() bool { return len(r.Missing) == 0 && len(r.Replaced) == 0 }

// WALCheckpoint is what PRAGMA wal_checkpoint returned: whether it was blocked,
// the frames in the log and the frames copied into the database file.
type WALCheckpoint struct {
	Busy         int `json:"busy"`
	Log          int `json:"log"`
	Checkpointed int `json:"checkpointed"`
}

// walFileChecker is implemented by repositories whose write-ahead log files
// can be checked; others are never checked.
type walFileChecker interface {
	CheckWALFiles() WALFileReport
	CheckpointWAL() (WALCheckpoint, error)
}

// DatabaseFileAlert reports that the database's write-ahead log files were
// deleted or replaced while the plugin had the database open. It lasts until
// the process stops or another database opens.
type DatabaseFileAlert struct {
	Path       string    `json:"path"`
	DetectedAt time.Time `json:"detected_at"`
	Missing    []string  `json:"missing,omitempty"`
	Replaced   []string  `json:"replaced,omitempty"`
	// The latest checkpoint the alert ran, and its error.
	CheckpointAt    time.Time      `json:"checkpoint_at,omitzero"`
	Checkpoint      *WALCheckpoint `json:"checkpoint,omitempty"`
	CheckpointError string         `json:"checkpoint_error,omitempty"`
}

// Files names the deleted or replaced files.
func (a DatabaseFileAlert) Files() string {
	return strings.Join(slices.Concat(a.Missing, a.Replaced), ", ")
}

type databaseFileWatch struct {
	mu         sync.Mutex
	generation uint64
	nextCheck  time.Time
	alert      *DatabaseFileAlert
	// checkpointFailed classifies the last checkpoint, so the log records
	// changes rather than every minute's result.
	checkpointFailed bool
	checkpointLogged bool
}

// reset forgets the alert of the previous database; callers hold Store.mu.
func (w *databaseFileWatch) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.generation++
	w.nextCheck = time.Time{}
	w.alert = nil
	w.checkpointFailed = false
	w.checkpointLogged = false
}

// CheckDatabaseFiles checks, at most once per DatabaseFileCheckInterval,
// whether the open database's -wal and -shm files were deleted or replaced, for
// example by an older sqlite3 closing the database while this process had lost
// its locks. It only stats those files. On the first finding it logs an error;
// while the alert lasts, every check checkpoints the deleted log into the
// database file, so a later kill loses as little as possible. It never reopens
// connections or creates or removes files: SQLite removes -wal and -shm by name
// when it closes, which could remove another process's files, so recovery is
// a graceful restart by the operator.
func (s *Store) CheckDatabaseFiles() {
	if s == nil {
		return
	}
	now := s.Now()
	watch := &s.databaseFiles
	watch.mu.Lock()
	if now.Before(watch.nextCheck) {
		watch.mu.Unlock()
		return
	}
	watch.nextCheck = now.Add(DatabaseFileCheckInterval)
	generation, alerted := watch.generation, watch.alert != nil
	watch.mu.Unlock()

	var (
		report     WALFileReport
		checkpoint WALCheckpoint
		errCheck   error
		path       string
	)
	s.mu.RLock()
	checker, ok := s.repo.(walFileChecker)
	if ok {
		path = s.path
		if !alerted {
			report = checker.CheckWALFiles()
		}
		if alerted || (report.Checked && !report.Healthy()) {
			checkpoint, errCheck = checker.CheckpointWAL()
		} else {
			ok = false
		}
	}
	s.mu.RUnlock()
	if !ok {
		return
	}

	watch.mu.Lock()
	if watch.generation != generation {
		// Another database opened meanwhile; this result belongs to the old one.
		watch.mu.Unlock()
		return
	}
	detected := watch.alert == nil
	if detected {
		watch.alert = &DatabaseFileAlert{Path: path, DetectedAt: now, Missing: report.Missing, Replaced: report.Replaced}
	}
	alert := watch.alert
	alert.CheckpointAt = now
	alert.Checkpoint, alert.CheckpointError = nil, ""
	if errCheck != nil {
		alert.CheckpointError = errCheck.Error()
	} else {
		result := checkpoint
		alert.Checkpoint = &result
	}
	failed := errCheck != nil || checkpoint.Busy != 0
	logCheckpoint := !watch.checkpointLogged || failed != watch.checkpointFailed
	watch.checkpointLogged, watch.checkpointFailed = true, failed
	files := alert.Files()
	watch.mu.Unlock()

	if detected {
		s.AddPluginLog(PluginLogError, "The write-ahead log files of billing database %s were deleted or replaced while the plugin had it open: %s. "+
			"Recent writes exist only in the deleted files, which this process keeps open; the plugin checkpoints them into the database file "+
			"every minute. Stop CLIProxyAPI gracefully (docker compose stop) and start it again; do not kill it. "+
			"Read the live database only with sqlite3 -readonly or a mode=ro URI", path, files)
	}
	if !logCheckpoint {
		return
	}
	switch {
	case errCheck != nil:
		s.AddPluginLog(PluginLogError, "Failed to checkpoint the deleted write-ahead log of billing database %s: %v", path, errCheck)
	case checkpoint.Busy != 0:
		s.AddPluginLog(PluginLogError, "Checkpointing the deleted write-ahead log of billing database %s was blocked: busy=%d, log=%d, checkpointed=%d; "+
			"it is retried every minute", path, checkpoint.Busy, checkpoint.Log, checkpoint.Checkpointed)
	default:
		s.AddPluginLog(PluginLogInfo, "Checkpointed the deleted write-ahead log of billing database %s into the database file: busy=%d, log=%d, checkpointed=%d; "+
			"it is checkpointed again every minute until CLIProxyAPI restarts", path, checkpoint.Busy, checkpoint.Log, checkpoint.Checkpointed)
	}
}

// DatabaseFileAlert returns the open database's write-ahead log alert.
func (s *Store) DatabaseFileAlert() (DatabaseFileAlert, bool) {
	if s == nil {
		return DatabaseFileAlert{}, false
	}
	s.databaseFiles.mu.Lock()
	defer s.databaseFiles.mu.Unlock()
	if s.databaseFiles.alert == nil {
		return DatabaseFileAlert{}, false
	}
	alert := *s.databaseFiles.alert
	alert.Missing, alert.Replaced = slices.Clone(alert.Missing), slices.Clone(alert.Replaced)
	if alert.Checkpoint != nil {
		checkpoint := *alert.Checkpoint
		alert.Checkpoint = &checkpoint
	}
	return alert, true
}
