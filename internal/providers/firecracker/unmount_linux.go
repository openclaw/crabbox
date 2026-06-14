//go:build linux

package firecracker

import (
	"errors"

	"golang.org/x/sys/unix"
)

func detachUnmount(path string) error {
	if err := unix.Unmount(path, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.EINVAL) {
		return err
	}
	return nil
}
