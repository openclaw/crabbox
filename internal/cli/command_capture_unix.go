//go:build !windows

package cli

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockCommandCaptureFile(file *os.File, writer bool) (bool, error) {
	mode := unix.LOCK_SH
	if writer {
		mode = unix.LOCK_EX
	}
	err := unix.Flock(int(file.Fd()), mode|unix.LOCK_NB)
	if !writer && errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}
