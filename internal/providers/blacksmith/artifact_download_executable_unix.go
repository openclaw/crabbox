//go:build darwin || linux

package blacksmith

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

func openBlacksmithDownloadExecutable(path string) (*os.File, error) {
	// Refuse replacement links and avoid blocking on a replacement FIFO before
	// the descriptor's regular-file check.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func checkBlacksmithDownloadCapabilities(file *os.File) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	_, err := unix.Fgetxattr(int(file.Fd()), "security.capability", nil)
	if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect artifact scp helper capabilities: %w", err)
	}
	return errors.New("artifact scp helper must not have Linux file capabilities")
}
