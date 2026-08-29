//go:build !linux && !darwin && !windows

package cli

import "fmt"

func controllerListenerOwnershipSupported() bool { return false }

func sshForwardRootListenerReady(port string, pid int) error {
	return controllerVerifyDaemonOwnedListener(port, pid)
}

func controllerVerifyDaemonOwnedListener(string, int) error {
	return fmt.Errorf("SSH tunnel listener ownership verification is unsupported on this platform")
}

func controllerVerifyDaemonOwnedListenerWithEnvironment(port string, pid int, _ []string) error {
	return controllerVerifyDaemonOwnedListener(port, pid)
}
