//go:build linux

package sqlite

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// SQLite's unix VFS lock bytes (os_unix.c): the SHARED range of the database
// file and the dead-man-switch byte of its -shm file.
const (
	sqliteSharedFirst = 0x40000000 + 2
	sqliteSharedSize  = 510
	sqliteShmDMS      = (22+8)*4 + 8
)

const lockProbeEnv = "CPA_BILLING_LOCK_PROBE"

// TestLockProbeHelper is the child process of heldByAnotherProcess: F_GETLK
// only reports locks other processes hold.
func TestLockProbeHelper(t *testing.T) {
	path := os.Getenv(lockProbeEnv)
	if path == "" {
		t.Skip("runs only as the child of heldByAnotherProcess")
	}
	start, _ := strconv.ParseInt(os.Getenv(lockProbeEnv+"_START"), 10, 64)
	length, _ := strconv.ParseInt(os.Getenv(lockProbeEnv+"_LEN"), 10, 64)
	// Opening the file here is harmless: this process holds no locks on it.
	file, err := os.Open(path)
	if err != nil {
		os.Stdout.WriteString("error: " + err.Error())
		os.Exit(0)
	}
	lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: start, Len: length}
	if err := syscall.FcntlFlock(file.Fd(), syscall.F_GETLK, &lock); err != nil {
		os.Stdout.WriteString("error: " + err.Error())
	} else if lock.Type == syscall.F_UNLCK {
		os.Stdout.WriteString("unlocked")
	} else {
		os.Stdout.WriteString("locked")
	}
	os.Exit(0)
}

// heldByAnotherProcess reports whether this process holds a lock on
// [start, start+length) of path that conflicts with a write lock.
func heldByAnotherProcess(t *testing.T, path string, start, length int64) bool {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestLockProbeHelper$")
	command.Env = append(os.Environ(), lockProbeEnv+"="+path,
		lockProbeEnv+"_START="+strconv.FormatInt(start, 10), lockProbeEnv+"_LEN="+strconv.FormatInt(length, 10))
	output, err := command.Output()
	if err != nil {
		t.Fatalf("lock probe: %v (%s)", err, output)
	}
	switch string(output) {
	case "locked":
		return true
	case "unlocked":
		return false
	}
	t.Fatalf("lock probe of %s: %s", path, output)
	return false
}

// SQLite before 3.51 deletes the live -wal and -shm files when it closes the
// database and finds no other process holding the database's SHARED lock, so
// this process must keep that lock, and the -shm lock, after opening both of
// its connections.
func TestOpenKeepsSQLiteLocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billing.db")
	database := openDatabase(t, path)
	references, err := database.OpenReferencePrices()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = references.Close() })
	if _, err := references.LoadReferencePriceMetadata(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustAppendPluginLog(t, database, eventStart, "info", "written", eventStart.Add(-1))

	if !heldByAnotherProcess(t, path, sqliteSharedFirst, sqliteSharedSize) {
		t.Errorf("no SHARED lock on %s: another process closing the database would checkpoint and delete the live -wal and -shm", path)
	}
	if !heldByAnotherProcess(t, path+"-shm", sqliteShmDMS, 1) {
		t.Errorf("no lock on the DMS byte of %s-shm", path)
	}
}
