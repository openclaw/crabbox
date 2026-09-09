//go:build linux

package cli

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func localCommandProcessGroup() int { return unix.Getpgrp() }

func observeLocalCommandLeader(pid, _ int) (bool, error) {
	var info unix.Siginfo
	err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
	if errors.Is(err, unix.EINTR) {
		return false, nil
	}
	if errors.Is(err, unix.ECHILD) {
		return false, fmt.Errorf("%w: waitid child %d: %w", errLocalCommandReservation, pid, err)
	}
	if err != nil {
		return false, fmt.Errorf("observe local command child %d: %w", pid, err)
	}
	return true, nil
}
