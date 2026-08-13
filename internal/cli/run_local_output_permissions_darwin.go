//go:build darwin

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

const darwinFchmodExtended = 283

func setPrivateFDPermissions(fd uintptr, mode os.FileMode) error {
	const (
		kauthUIDNone  = ^uint32(0) - 100
		kauthGIDNone  = ^uint32(0) - 100
		removeFileACL = uintptr(1)
	)
	_, _, err := unix.Syscall6(
		darwinFchmodExtended,
		fd,
		uintptr(kauthUIDNone),
		uintptr(kauthGIDNone),
		uintptr(mode.Perm()),
		removeFileACL,
		0,
	)
	if err != 0 {
		return err
	}
	return nil
}
