package sqlite

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestSecureFilesCreatesPrivateDatabase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	path := filepath.Join(t.TempDir(), "billing.db")
	if err := secureFiles(path); err != nil {
		t.Fatal(err)
	}
	if mode := fileMode(t, path); mode != 0o600 {
		t.Fatalf("new database mode = %o, want 600", mode)
	}
}

func TestSecureFilesMakesExistingFilesPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	path := filepath.Join(t.TempDir(), "billing.db")
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.WriteFile(name, []byte("kept"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(name, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := secureFiles(path); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		if mode := fileMode(t, name); mode != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, mode)
		}
		if content, err := os.ReadFile(name); err != nil || string(content) != "kept" {
			t.Errorf("%s = %q, %v; secureFiles must leave existing files as they are", name, content, err)
		}
	}
}

func TestSecureFilesIgnoresMissingWALFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billing.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := secureFiles(path); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Errorf("secureFiles created %s", name)
		}
	}
}
