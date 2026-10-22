//go:build !windows

package cli

import (
	"errors"
	"syscall"
)

func coordinatorReadConnectionInterrupted(err error) bool {
	return errors.Is(err, syscall.ECONNRESET)
}
