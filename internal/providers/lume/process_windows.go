//go:build windows

package lume

import (
	"fmt"
	"os"
	"os/exec"
)

func detachCommand(_ *exec.Cmd) {}

func processAlive(_ int) bool { return false }

func signalProcessInterrupt(pid int) error {
	return fmt.Errorf("interrupting Lume owner pid %d is unsupported on Windows", pid)
}

func tryExclusiveFileLock(_ *os.File) (bool, error) {
	return false, fmt.Errorf("locking Lume VM configuration is unsupported on Windows")
}

func unlockFile(_ *os.File) error { return nil }
