package database

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProcessLock(t *testing.T) {
	db := filepath.Join(t.TempDir(), "db.sqlite")
	l, err := AcquireProcessLock(db)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(db + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode: %o", info.Mode().Perm())
	}
	if _, err := AcquireProcessLock(db); err == nil {
		t.Fatal("second lock acquired")
	}
	other, err := AcquireProcessLock(db + "other")
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(db + ".lock"); err != nil {
		t.Fatal("lock file was removed", err)
	}
	next, err := AcquireProcessLock(db)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}
