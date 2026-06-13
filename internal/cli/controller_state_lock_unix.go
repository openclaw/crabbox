//go:build linux || darwin

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

type controllerStateLock struct {
	file *os.File
	once sync.Once
	err  error
}

func acquireControllerStateLock(statePath string) (*controllerStateLock, error) {
	dir := filepath.Clean(filepath.Dir(statePath))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create controller state directory for lock: %w", err)
	}
	dirFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open controller state directory for lock: %w", err)
	}
	defer unix.Close(dirFD)
	var dirStat unix.Stat_t
	if err := unix.Fstat(dirFD, &dirStat); err != nil {
		return nil, fmt.Errorf("inspect controller state directory for lock: %w", err)
	}
	if err := validateControllerStateDirectoryStat(&dirStat); err != nil {
		return nil, err
	}

	lockName := filepath.Base(statePath) + ".lock"
	lockFD, err := unix.Openat(dirFD, lockName, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open controller state lock: %w", err)
	}
	lockFile := os.NewFile(uintptr(lockFD), filepath.Join(dir, lockName))
	if lockFile == nil {
		_ = unix.Close(lockFD)
		return nil, fmt.Errorf("open controller state lock: invalid file descriptor")
	}
	closeLock := true
	defer func() {
		if closeLock {
			_ = lockFile.Close()
		}
	}()

	var lockStat unix.Stat_t
	if err := unix.Fstat(lockFD, &lockStat); err != nil {
		return nil, fmt.Errorf("inspect controller state lock: %w", err)
	}
	if err := validateControllerStateLockStat(&lockStat); err != nil {
		return nil, err
	}
	if err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("controller state is already locked by another process")
		}
		return nil, fmt.Errorf("lock controller state: %w", err)
	}
	closeLock = false
	return &controllerStateLock{file: lockFile}, nil
}

func validateControllerStateDirectoryStat(stat *unix.Stat_t) error {
	if stat == nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("controller state directory must be a directory")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("controller state directory must be owned by the current user")
	}
	if stat.Mode&0o022 != 0 {
		return fmt.Errorf("controller state directory must not be writable by group or others")
	}
	return nil
}

func validateControllerStateDirectoryPath(dir string) error {
	dirFD, err := unix.Open(filepath.Clean(dir), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open controller state directory: %w", err)
	}
	defer unix.Close(dirFD)
	var stat unix.Stat_t
	if err := unix.Fstat(dirFD, &stat); err != nil {
		return fmt.Errorf("inspect controller state directory: %w", err)
	}
	return validateControllerStateDirectoryStat(&stat)
}

func validateControllerStateLockStat(stat *unix.Stat_t) error {
	if stat == nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("controller state lock must be a regular file")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("controller state lock must be owned by the current user")
	}
	if stat.Mode&0o077 != 0 {
		return fmt.Errorf("controller state lock must not be accessible by group or others")
	}
	return nil
}

func (l *controllerStateLock) Unlock() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if l.file == nil {
			return
		}
		l.err = errors.Join(unix.Flock(int(l.file.Fd()), unix.LOCK_UN), l.file.Close())
	})
	return l.err
}
