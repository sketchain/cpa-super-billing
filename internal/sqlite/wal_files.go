package sqlite

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"cpa-super-billing/internal/billing"
)

// walFileRecord identifies the -wal and -shm files of an open database.
type walFileRecord struct {
	wal, shm os.FileInfo
}

// recordWALFiles identifies the -wal and -shm files once the database is open
// in WAL mode, so CheckWALFiles can tell when they are deleted or replaced. It
// only stats the files: opening and closing them would release this process's
// SQLite locks on them. Windows cannot delete files that are open, so the check
// stays off there.
func (d *DB) recordWALFiles() {
	if runtime.GOOS == "windows" {
		return
	}
	wal, errWAL := os.Stat(d.path + "-wal")
	shm, errSHM := os.Stat(d.path + "-shm")
	if errWAL != nil || errSHM != nil {
		return
	}
	d.walFiles = &walFileRecord{wal: wal, shm: shm}
}

// CheckWALFiles reports whether the -wal and -shm files this database opened
// with are still the ones on disk. It never opens them.
func (d *DB) CheckWALFiles() billing.WALFileReport {
	if d.walFiles == nil {
		return billing.WALFileReport{}
	}
	report := billing.WALFileReport{Checked: true}
	for _, file := range []struct {
		name     string
		recorded os.FileInfo
	}{{d.path + "-wal", d.walFiles.wal}, {d.path + "-shm", d.walFiles.shm}} {
		current, err := os.Stat(file.name)
		switch {
		case errors.Is(err, os.ErrNotExist):
			report.Missing = append(report.Missing, file.name)
		case err != nil:
			// An unreadable directory says nothing about the files themselves.
		case !os.SameFile(file.recorded, current):
			report.Replaced = append(report.Replaced, file.name)
		}
	}
	return report
}

// CheckpointWAL copies everything the write-ahead log of this connection holds
// into the database file and truncates the log. After the live -wal file was
// deleted, this connection still writes to the deleted file, and only a
// checkpoint puts those writes into the database file, which still exists.
func (d *DB) CheckpointWAL() (billing.WALCheckpoint, error) {
	var result billing.WALCheckpoint
	if err := d.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&result.Busy, &result.Log, &result.Checkpointed); err != nil {
		return result, fmt.Errorf("Checkpoint billing database %s: %w", d.path, err)
	}
	return result, nil
}
