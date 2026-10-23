//go:build !windows

package cli

import (
	"errors"
	"syscall"
)

func coordinatorReadConnectionInterrupted(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE)
}
