//go:build windows

package lume

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

func detachCommand(_ *exec.Cmd) {}

func processAlive(_ int) bool { return false }

func signalProcessInterrupt(pid int) error {
	return fmt.Errorf("interrupting Lume owner pid %d is unsupported on Windows", pid)
}

func tryExclusiveFileLock(file *os.File) (bool, error) {
	overlapped := lumeConfigLockOverlapped()
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&overlapped,
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

func unlockFile(file *os.File) error {
	overlapped := lumeConfigLockOverlapped()
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}

func lumeConfigLockOverlapped() windows.Overlapped {
	return windows.Overlapped{Offset: ^uint32(0), OffsetHigh: 0x7fffffff}
}

func openLumeConfigForLock(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, windows.ERROR_INVALID_HANDLE
	}
	return file, nil
}

func prepareLumeConfigForDirectoryRename(file *os.File) error {
	if err := unlockFile(file); err != nil && !errors.Is(err, windows.ERROR_NOT_LOCKED) {
		return err
	}
	return file.Close()
}

func relockLumeConfigAfterDirectoryRename(path string) (*os.File, bool, error) {
	file, err := openLumeConfigForLock(path)
	if err != nil {
		return nil, false, err
	}
	locked, err := tryExclusiveFileLock(file)
	if err != nil || !locked {
		_ = file.Close()
		return nil, locked, err
	}
	return file, true, nil
}
