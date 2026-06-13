//go:build !linux && !darwin && !windows

package cli

import "fmt"

func controllerListenerOwnershipSupported() bool { return false }

func controllerVerifyDaemonOwnedListener(string, int) error {
	return fmt.Errorf("SSH tunnel listener ownership verification is unsupported on this platform")
}
