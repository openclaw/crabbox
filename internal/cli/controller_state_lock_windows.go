//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/windows"
)

type controllerStateLock struct {
	file       *os.File
	overlapped windows.Overlapped
	once       sync.Once
	err        error
}

func acquireControllerStateLock(statePath string) (*controllerStateLock, error) {
	dir := filepath.Clean(filepath.Dir(statePath))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create controller state directory for lock: %w", err)
	}
	lockPath := filepath.Join(dir, filepath.Base(statePath)+".lock")
	lockFile, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open controller state lock: %w", err)
	}
	lock := &controllerStateLock{file: lockFile}
	if err := windows.LockFileEx(
		windows.Handle(lockFile.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&lock.overlapped,
	); err != nil {
		_ = lockFile.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, fmt.Errorf("controller state is already locked by another process")
		}
		return nil, fmt.Errorf("lock controller state: %w", err)
	}
	return lock, nil
}

func validateControllerStateDirectoryPath(dir string) error {
	info, err := os.Stat(filepath.Clean(dir))
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("controller state directory must be a directory")
	}
	return nil
}

func (l *controllerStateLock) Unlock() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		l.err = errors.Join(
			windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &l.overlapped),
			l.file.Close(),
		)
	})
	return l.err
}
