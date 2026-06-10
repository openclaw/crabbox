//go:build windows

package cli

import (
	"syscall"
	"unsafe"
)

var moveFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func replaceClaimFile(tmpPath, path string) error {
	oldPath, err := syscall.UTF16PtrFromString(tmpPath)
	if err != nil {
		return err
	}
	newPath, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	const (
		movefileReplaceExisting = 0x1
		movefileWriteThrough    = 0x8
	)
	r1, _, callErr := moveFileExW.Call(
		uintptr(unsafe.Pointer(oldPath)),
		uintptr(unsafe.Pointer(newPath)),
		uintptr(movefileReplaceExisting|movefileWriteThrough),
	)
	if r1 != 0 {
		return nil
	}
	if callErr != syscall.Errno(0) {
		return callErr
	}
	return syscall.EINVAL
}
