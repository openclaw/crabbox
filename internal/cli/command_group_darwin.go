//go:build darwin

package cli

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func localCommandProcessGroup() int { return unix.Getpgrp() }

func observeLocalCommandLeader(pid, parent int) (bool, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if errors.Is(err, unix.EINTR) {
		return false, nil
	}
	if errors.Is(err, unix.EIO) || errors.Is(err, unix.ESRCH) {
		return false, fmt.Errorf("%w: inspect child %d: %w", errLocalCommandReservation, pid, err)
	}
	if err != nil {
		return false, fmt.Errorf("observe local command child %d: %w", pid, err)
	}
	if info.Proc.P_pid != int32(pid) || info.Eproc.Ppid != int32(parent) {
		return false, fmt.Errorf("%w: child %d no longer belongs to parent %d", errLocalCommandReservation, pid, parent)
	}
	// Darwin SZOMB is exited but still awaiting its parent's collecting wait.
	return info.Proc.P_stat == 5, nil
}
