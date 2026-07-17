//go:build windows

package lume

import (
	"fmt"
	"os/exec"
)

func detachCommand(_ *exec.Cmd) {}

func processAlive(_ int) bool { return false }

func signalProcessInterrupt(pid int) error {
	return fmt.Errorf("interrupting Lume owner pid %d is unsupported on Windows", pid)
}
