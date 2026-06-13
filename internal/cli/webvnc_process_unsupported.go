//go:build !darwin && !linux && !windows

package cli

import "fmt"

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
	return "", fmt.Errorf("WebVNC daemon process identity is unsupported on this platform for pid %d", pid)
}
