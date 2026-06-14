//go:build !windows

package cli

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openControllerTokenFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("inspect token file owner: %w", err)
	}
	if err := validateControllerTokenFileOwner(&stat); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("create token file handle")
	}
	return file, nil
}

func validateControllerTokenFileOwner(stat *unix.Stat_t) error {
	if stat == nil || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("controller token file must be owned by the current user")
	}
	return nil
}
