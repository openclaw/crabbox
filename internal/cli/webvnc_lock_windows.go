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
	// The bound listener is registered with Go's IOCP poller, and net's own
	// dupFileSocket documents that a handle associated with IOCP is not safe to
	// share with another process: the child's WSADuplicateSocket fails with
	// ERROR_INVALID_PARAMETER. TCPListener.File hands back a fresh Winsock
	// duplicate that was never registered with the poller, so that is the handle
	// the child can adopt through net.FileListener. The caller keeps the file
	// open until handoff so the kernel reservation stays live.
	file, err := listener.File()
	if err != nil {
		return "", nil, err
	}
	handle := windows.Handle(file.Fd())
	if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		_ = file.Close()
		return "", nil, err
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.AdditionalInheritedHandles = append(
		cmd.SysProcAttr.AdditionalInheritedHandles,
		syscall.Handle(handle),
	)
	return strconv.FormatUint(uint64(handle), 10), file, nil
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

func tryLockWebVNCDaemonFile(file *os.File) (bool, error) {
	var overlapped windows.Overlapped
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

func unlockWebVNCDaemonFile(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}
