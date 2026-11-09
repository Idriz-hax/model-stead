package database

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

type ProcessLock struct {
	file *os.File
	one  sync.Once
	err  error
}

func AcquireProcessLock(databasePath string) (*ProcessLock, error) {
	path := filepath.Clean(databasePath) + ".lock"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open database lock: %w", err)
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, fmt.Errorf("secure database lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("database is already in use")
	}
	return &ProcessLock{file: f}, nil
}

func (l *ProcessLock) Close() error {
	if l == nil {
		return nil
	}
	l.one.Do(func() {
		if err := unix.Flock(int(l.file.Fd()), unix.LOCK_UN); err != nil {
			l.err = err
		}
		if err := l.file.Close(); l.err == nil {
			l.err = err
		}
	})
	return l.err
}
