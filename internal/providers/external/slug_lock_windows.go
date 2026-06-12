//go:build windows

package external

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func lockSlugReservation(path string) (func(), bool, error) {
	lockPath, err := slugReservationLockPath(path)
	if err != nil {
		return nil, false, err
	}
	token, err := newSlugReservationToken()
	if err != nil {
		return nil, false, err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		reclaimed, err := reclaimStaleWindowsSlugLock(lockPath)
		if err != nil {
			return nil, false, err
		}
		if !reclaimed {
			return nil, false, nil
		}
		lock, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	}
	if errors.Is(err, os.ErrExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("lock external slug reservation: %w", err)
	}
	if _, err := lock.WriteString(token); err != nil {
		_ = lock.Close()
		_ = removeOwnedWindowsSlugLock(lockPath, token)
		return nil, false, fmt.Errorf("write external slug reservation lock: %w", err)
	}
	return func() {
		_ = lock.Close()
		_ = removeOwnedWindowsSlugLock(lockPath, token)
	}, true, nil
}

func reclaimStaleWindowsSlugLock(lockPath string) (bool, error) {
	// Go opens files on Windows without FILE_SHARE_DELETE, so a live holder's
	// handle makes removal fail. A successful remove means the lock is abandoned.
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		if isActiveWindowsSlugLock(err) {
			return false, nil
		}
		return false, fmt.Errorf("remove stale external slug reservation lock: %w", err)
	}
	return true, nil
}

func isActiveWindowsSlugLock(err error) bool {
	return os.IsPermission(err) ||
		errors.Is(err, syscall.Errno(32)) ||
		errors.Is(err, syscall.Errno(33))
}

func removeOwnedWindowsSlugLock(lockPath, token string) error {
	owner, err := os.ReadFile(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read external slug reservation lock: %w", err)
	}
	if string(owner) != token {
		return nil
	}
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove external slug reservation lock: %w", err)
	}
	return nil
}
