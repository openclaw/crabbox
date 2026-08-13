//go:build !windows && !darwin

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

func setPrivateFDPermissions(fd uintptr, mode os.FileMode) error {
	return unix.Fchmod(int(fd), uint32(mode.Perm()))
}
