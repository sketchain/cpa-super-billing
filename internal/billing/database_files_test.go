package billing

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// walRepository is a memory repository whose write-ahead log files can be
// reported deleted and whose checkpoints can fail.
type walRepository struct {
	*memoryRepository
	report      WALFileReport
	checks      int
	checkpoints int
	checkpoint  WALCheckpoint
	checkFail   error
}

func (r *walRepository) CheckWALFiles() WALFileReport {
	r.checks++
	return r.report
}

func (r *walRepository) CheckpointWAL() (WALCheckpoint, error) {
	r.checkpoints++
	return r.checkpoint, r.checkFail
}

func newWALStore(t *testing.T) (*Store, *walRepository, *time.Time) {
	t.Helper()
	repo := &walRepository{memoryRepository: &memoryRepository{}, report: WALFileReport{Checked: true}}
	store := NewStore(func(string) (Repository, error) { return repo, nil }, nil)
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(testConfig(t)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store, repo, &now
}

func walLogs(repo *walRepository) (errors, infos []string) {
	for _, entry := range repo.pluginLogs {
		if !strings.Contains(entry.Message, "write-ahead log") {
			continue
		}
		if entry.Level == PluginLogError {
			errors = append(errors, entry.Message)
		} else {
			infos = append(infos, entry.Message)
		}
	}
	return errors, infos
}

func TestCheckDatabaseFilesIsThrottled(t *testing.T) {
	store, repo, now := newWALStore(t)
	for range 3 {
		store.CheckDatabaseFiles()
	}
	*now = now.Add(DatabaseFileCheckInterval - time.Second)
	store.CheckDatabaseFiles()
	if repo.checks != 1 || repo.checkpoints != 0 {
		t.Fatalf("checks = %d, checkpoints = %d; want one check and no checkpoint", repo.checks, repo.checkpoints)
	}
	*now = now.Add(time.Second)
	store.CheckDatabaseFiles()
	if repo.checks != 2 {
		t.Fatalf("checks after the interval = %d, want 2", repo.checks)
	}
	if _, ok := store.DatabaseFileAlert(); ok {
		t.Fatal("alert on healthy files")
	}
}

func TestCheckDatabaseFilesLogsOnceAndCheckpointsEveryInterval(t *testing.T) {
	store, repo, now := newWALStore(t)
	repo.report = WALFileReport{Checked: true, Missing: []string{"/data/billing.db-wal"}, Replaced: []string{"/data/billing.db-shm"}}
	repo.checkpoint = WALCheckpoint{Busy: 1, Log: 9, Checkpointed: 4}
	store.CheckDatabaseFiles()
	alert, ok := store.DatabaseFileAlert()
	if !ok || alert.Files() != "/data/billing.db-wal, /data/billing.db-shm" || alert.Checkpoint == nil || alert.Checkpoint.Busy != 1 {
		t.Fatalf("alert = %+v, %v", alert, ok)
	}
	errs, infos := walLogs(repo)
	if len(errs) != 2 || len(infos) != 0 || !strings.Contains(errs[1], "busy=1, log=9, checkpointed=4") {
		t.Fatalf("logs after a blocked checkpoint: errors %q, infos %q", errs, infos)
	}

	// The same blocked result is not logged again.
	*now = now.Add(DatabaseFileCheckInterval)
	store.CheckDatabaseFiles()
	if errs, _ := walLogs(repo); len(errs) != 2 {
		t.Fatalf("a repeated blocked checkpoint logged again: %q", errs)
	}
	// A failure, then success, are each logged once.
	*now = now.Add(DatabaseFileCheckInterval)
	repo.checkFail = errors.New("disk I/O error")
	store.CheckDatabaseFiles()
	*now = now.Add(DatabaseFileCheckInterval)
	repo.checkFail, repo.checkpoint = nil, WALCheckpoint{Log: 9, Checkpointed: 9}
	store.CheckDatabaseFiles()
	*now = now.Add(DatabaseFileCheckInterval)
	store.CheckDatabaseFiles()
	errs, infos = walLogs(repo)
	if len(errs) != 2 || len(infos) != 1 || !strings.Contains(infos[0], "busy=0, log=9, checkpointed=9") {
		t.Fatalf("logs after recovering: errors %q, infos %q", errs, infos)
	}
	if repo.checkpoints != 5 || repo.checks != 1 {
		t.Fatalf("checkpoints = %d, checks = %d; want a checkpoint every interval and no new file check", repo.checkpoints, repo.checks)
	}
	if alert, _ := store.DatabaseFileAlert(); alert.CheckpointError != "" || alert.Checkpoint.Checkpointed != 9 ||
		!alert.DetectedAt.Equal(now.Add(-4*DatabaseFileCheckInterval)) {
		t.Fatalf("alert after recovering = %+v", alert)
	}
}

func TestCheckDatabaseFilesSkipsUncheckedDatabases(t *testing.T) {
	store, repo, _ := newWALStore(t)
	repo.report = WALFileReport{Missing: []string{"ignored: not checked"}}
	store.CheckDatabaseFiles()
	if _, ok := store.DatabaseFileAlert(); ok || repo.checkpoints != 0 {
		t.Fatalf("an unchecked report raised the alert")
	}
}
