//go:build !windows

package cli

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"

	"golang.org/x/sys/unix"
)

func webVNCDaemonPortReservationUnavailable(err error) bool {
	return errors.Is(err, unix.EADDRINUSE)
}

func inheritWebVNCDaemonPortReservation(cmd *exec.Cmd, listener *net.TCPListener) (string, *os.File, error) {
	file, err := listener.File()
	if err != nil {
		return "", nil, err
	}
	descriptor := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, file)
	return strconv.Itoa(descriptor), file, nil
}

func lockWebVNCDaemonFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX)
}

func unlockWebVNCDaemonFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
