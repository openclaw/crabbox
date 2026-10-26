package cli

import (
	"errors"
	"syscall"
)

func coordinatorReadConnectionInterrupted(err error) bool {
	// Windows socket errors are distinct from syscall's invented POSIX values.
	// Read-only requests may recover with a new connection within their existing budget.
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.WSAECONNRESET) || errors.Is(err, syscall.WSAECONNABORTED)
}
