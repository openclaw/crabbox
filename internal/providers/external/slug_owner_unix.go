//go:build !windows

package external

import (
	"errors"
	"syscall"
)

func slugReservationOwnerActive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
