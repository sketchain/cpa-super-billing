package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"cpa-super-billing/internal/billing"
)

func skipWALCheckOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the write-ahead log file check is off on Windows, which cannot delete open files")
	}
}

func assertHealthy(t *testing.T, database *DB, when string) {
	t.Helper()
	report := database.CheckWALFiles()
	if !report.Checked || !report.Healthy() {
		t.Fatalf("%s: CheckWALFiles = %+v, want checked and healthy", when, report)
	}
}

// Everything the plugin does while it runs keeps the -wal and -shm files it
// opened with: writes, checkpoints the log restarts after, the reference price
// connection, backups and copies.
func TestCheckWALFilesStaysHealthyInNormalUse(t *testing.T) {
	skipWALCheckOnWindows(t)
	path := filepath.Join(t.TempDir(), "billing.db")
	seedV24(t, path)
	// Opening migrates, after a VACUUM INTO backup.
	database := openWith(t, path, withCC())
	if names := backupNames(t, path); len(names) != 1 {
		t.Fatalf("migration backups = %v, want one", names)
	}
	assertHealthy(t, database, "after migrating")

	references, err := database.OpenReferencePrices()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = references.Close() })
	if _, err := references.LoadReferencePriceMetadata(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := references.SearchReferencePrices(context.Background(), "gpt", 5); err != nil {
		t.Fatal(err)
	}
	assertHealthy(t, database, "after the reference price connection opened")

	if _, err := database.db.Exec("PRAGMA wal_autocheckpoint = 2"); err != nil {
		t.Fatal(err)
	}
	for index := range 200 {
		mustAppendPluginLog(t, database, eventStart.Add(timeSecond(index)), billing.PluginLogInfo, "auto checkpoint", eventStart.Add(-1))
	}
	for _, mode := range []string{"PASSIVE", "FULL", "RESTART", "TRUNCATE"} {
		if _, err := database.db.Exec("PRAGMA wal_checkpoint(" + mode + ")"); err != nil {
			t.Fatal(err)
		}
		mustAppendPluginLog(t, database, eventStart, billing.PluginLogInfo, mode, eventStart.Add(-1))
	}
	assertHealthy(t, database, "after checkpoints")

	if err := vacuumInto(database.db, filepath.Join(filepath.Dir(path), "backup.db"), schemaVersion); err != nil {
		t.Fatal(err)
	}
	if err := CopyDatabase(path, filepath.Join(filepath.Dir(path), "copy.db")); err != nil {
		t.Fatal(err)
	}
	assertHealthy(t, database, "after backing up and copying the database")
}

func TestCheckWALFilesReportsDeletedAndReplacedFiles(t *testing.T) {
	skipWALCheckOnWindows(t)
	path := filepath.Join(t.TempDir(), "billing.db")
	database := openDatabase(t, path)
	if err := os.Remove(path + "-wal"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + "-shm"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-shm", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	report := database.CheckWALFiles()
	if !report.Checked || !slices.Equal(report.Missing, []string{path + "-wal"}) || !slices.Equal(report.Replaced, []string{path + "-shm"}) {
		t.Fatalf("CheckWALFiles = %+v, want the -wal missing and the -shm replaced", report)
	}
}

// After the live -wal and -shm are deleted, writes go to the deleted log; a
// checkpoint copies them into the database file and creates no files.
func TestCheckpointWALSavesWritesFromDeletedLog(t *testing.T) {
	skipWALCheckOnWindows(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "billing.db")
	database := openDatabase(t, path)
	mustAppendPluginLog(t, database, eventStart, billing.PluginLogInfo, "before", eventStart.Add(-1))
	for _, name := range []string{path + "-wal", path + "-shm"} {
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
	for index := range 20 {
		mustAppendPluginLog(t, database, eventStart.Add(timeSecond(index+1)), billing.PluginLogInfo, "into the deleted log", eventStart.Add(-1))
	}
	checkpoint, err := database.CheckpointWAL()
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Busy != 0 || checkpoint.Checkpointed != checkpoint.Log {
		t.Fatalf("CheckpointWAL = %+v, want every frame copied", checkpoint)
	}
	for _, name := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Errorf("CheckpointWAL created %s", name)
		}
	}
	// The database file alone, without any log, holds every write.
	copied := filepath.Join(t.TempDir(), "copied.db")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copied, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if count := intQuery(t, readOnly(t, copied), "SELECT count(*) FROM plugin_logs"); count != 21 {
		t.Fatalf("plugin logs in the database file = %d, want 21", count)
	}
}

func timeSecond(index int) time.Duration { return time.Duration(index) * time.Second }
