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

func inspectProcessSnapshot(pid int) (processSnapshot, error) {
	if pid <= 0 {
		return processSnapshot{}, fmt.Errorf("pid must be positive")
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return processSnapshot{}, err
	}
	if info.Proc.P_pid != int32(pid) {
		return processSnapshot{}, fmt.Errorf("process %d identity unavailable", pid)
	}
	started := info.Proc.P_starttime
	if started.Sec <= 0 || started.Usec < 0 {
		return processSnapshot{}, fmt.Errorf("process %d start identity unavailable", pid)
	}
	// Darwin sys/proc.h defines SZOMB as 5: exited, awaiting its parent's wait.
	return processSnapshot{started: fmt.Sprintf("%d.%06d", started.Sec, started.Usec), exited: info.Proc.P_stat == 5}, nil
}
