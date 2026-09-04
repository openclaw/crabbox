//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

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

func forwardInheritedWebVNCDaemonPortReservation(cmd *exec.Cmd) (func(), error) {
	port := strings.TrimSpace(os.Getenv(webVNCDaemonPortReservationEnv))
	descriptor := strings.TrimSpace(os.Getenv(webVNCDaemonPortReservationFDEnv))
	if port == "" && descriptor == "" {
		return func() {}, nil
	}
	fd, err := strconv.Atoi(descriptor)
	if err != nil || fd <= 2 {
		return nil, fmt.Errorf("inherited WebVNC daemon TCP listener descriptor is invalid")
	}
	duplicate, err := unix.Dup(fd)
	if err != nil {
		return nil, fmt.Errorf("duplicate inherited WebVNC daemon TCP listener: %w", err)
	}
	file := os.NewFile(uintptr(duplicate), "crabbox-webvnc-supervisor-listener")
	if file == nil {
		_ = unix.Close(duplicate)
		return nil, fmt.Errorf("open inherited WebVNC daemon TCP listener")
	}
	childDescriptor := strconv.Itoa(3 + len(cmd.ExtraFiles))
	cmd.ExtraFiles = append(cmd.ExtraFiles, file)
	cmd.Env = webVNCDaemonPortReservationEnvironment(cmd.Env, port, childDescriptor)
	return func() { _ = file.Close() }, nil
}

func tryLockWebVNCDaemonFile(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

func unlockWebVNCDaemonFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
