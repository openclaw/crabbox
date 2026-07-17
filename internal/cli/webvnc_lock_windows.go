//go:build windows

package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func webVNCDaemonPortReservationUnavailable(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, windows.WSAEACCES)
}

func inheritWebVNCDaemonPortReservation(cmd *exec.Cmd, listener *net.TCPListener) (string, *os.File, error) {
	// TCPListener.File returns a Winsock duplicate that Go explicitly forbids
	// using in another process. Transfer the original bound socket handle; the
	// child immediately makes its own Winsock duplicate through net.FileListener.
	raw, err := listener.SyscallConn()
	if err != nil {
		return "", nil, err
	}
	var handle windows.Handle
	var inheritErr error
	if err := raw.Control(func(fd uintptr) {
		handle = windows.Handle(fd)
		inheritErr = windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT)
	}); err != nil {
		return "", nil, err
	}
	if inheritErr != nil {
		return "", nil, inheritErr
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.AdditionalInheritedHandles = append(
		cmd.SysProcAttr.AdditionalInheritedHandles,
		syscall.Handle(handle),
	)
	return strconv.FormatUint(uint64(handle), 10), nil, nil
}

func forwardInheritedWebVNCDaemonPortReservation(cmd *exec.Cmd) (func(), error) {
	port := strings.TrimSpace(os.Getenv(webVNCDaemonPortReservationEnv))
	descriptor := strings.TrimSpace(os.Getenv(webVNCDaemonPortReservationFDEnv))
	if port == "" && descriptor == "" {
		return func() {}, nil
	}
	handleValue, err := strconv.ParseUint(descriptor, 10, 64)
	if err != nil || handleValue == 0 {
		return nil, fmt.Errorf("inherited WebVNC daemon TCP listener handle is invalid")
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.AdditionalInheritedHandles = append(cmd.SysProcAttr.AdditionalInheritedHandles, syscall.Handle(handleValue))
	cmd.Env = webVNCDaemonPortReservationEnvironment(cmd.Env, port, descriptor)
	return func() {}, nil
}

func lockWebVNCDaemonFile(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped)
}

func unlockWebVNCDaemonFile(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}
