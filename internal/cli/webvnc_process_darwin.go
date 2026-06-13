//go:build darwin

package cli

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func processBootIdentity() (string, error) {
	return "", nil
}

func processBootIdentityRequired() bool {
	return false
}

func validPersistedProcessBootIdentity(string) bool {
	return true
}

func webVNCDaemonProcessStartIdentity(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("pid must be positive")
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if info.Proc.P_pid != int32(pid) {
		return "", fmt.Errorf("process %d identity unavailable", pid)
	}
	started := info.Proc.P_starttime
	if started.Sec <= 0 || started.Usec < 0 {
		return "", fmt.Errorf("process %d start identity unavailable", pid)
	}
	return fmt.Sprintf("%d.%06d", started.Sec, started.Usec), nil
}
