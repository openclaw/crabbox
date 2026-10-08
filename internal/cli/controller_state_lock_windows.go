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
	if err := secureControllerStateDirectoryPath(dir); err != nil {
		return nil, err
	}

	dirHandle, err := openControllerStateWindowsHandle(dir, true, windows.OPEN_EXISTING)
	if err != nil {
		return nil, fmt.Errorf("open controller state directory for lock: %w", err)
	}
	defer windows.CloseHandle(dirHandle)
	if err := validateControllerStateWindowsHandle(dirHandle, true); err != nil {
		return nil, err
	}

	lockPath := filepath.Join(dir, filepath.Base(statePath)+".lock")
	lockHandle, err := openControllerStateWindowsHandle(lockPath, false, windows.OPEN_ALWAYS)
	if err != nil {
		return nil, fmt.Errorf("open controller state lock: %w", err)
	}
	lockFile := os.NewFile(uintptr(lockHandle), lockPath)
	if lockFile == nil {
		_ = windows.CloseHandle(lockHandle)
		return nil, fmt.Errorf("open controller state lock: invalid file handle")
	}
	lock := &controllerStateLock{file: lockFile}
	closeLock := true
	defer func() {
		if closeLock {
			_ = lockFile.Close()
		}
	}()
	if err := validateControllerStateWindowsHandle(lockHandle, false); err != nil {
		return nil, err
	}
	if err := windows.LockFileEx(
		lockHandle,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&lock.overlapped,
	); err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, fmt.Errorf("controller state is already locked by another process")
		}
		return nil, fmt.Errorf("lock controller state: %w", err)
	}
	closeLock = false
	return lock, nil
}

func secureControllerStateDirectoryPath(dir string) error {
	if err := secureSSHTransportPath(dir, true); err != nil {
		return fmt.Errorf("secure controller state directory: %w", err)
	}
	return nil
}

func validateControllerStateDirectoryPath(dir string) error {
	handle, err := openControllerStateWindowsHandle(filepath.Clean(dir), true, windows.OPEN_EXISTING)
	if err != nil {
		return fmt.Errorf("open controller state directory: %w", err)
	}
	defer windows.CloseHandle(handle)
	return validateControllerStateWindowsHandle(handle, true)
}

func openControllerStateWindowsHandle(path string, directory bool, disposition uint32) (windows.Handle, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	return windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		disposition,
		flags,
		0,
	)
}

func validateControllerStateWindowsHandle(handle windows.Handle, directory bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("inspect controller state path: %w", err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("controller state path must not be a reparse point")
	}
	isDirectory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if directory != isDirectory {
		if directory {
			return fmt.Errorf("controller state directory must be a directory")
		}
		return fmt.Errorf("controller state lock must be a regular file")
	}
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("inspect controller state owner: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("read controller state owner: %w", err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current controller user: %w", err)
	}
	if owner == nil || user == nil || user.User.Sid == nil || !owner.Equals(user.User.Sid) {
		return fmt.Errorf("controller state path must be owned by the current user")
	}
	if err := validateExternalRoutingWindowsPrivateDACL(descriptor, user.User.Sid); err != nil {
		return fmt.Errorf("controller state path: %w", err)
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
		l.err = errors.Join(
			windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &l.overlapped),
			l.file.Close(),
		)
	})
	return l.err
}
