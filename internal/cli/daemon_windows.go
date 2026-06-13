//go:build windows

package cli

import (
	"os"
	"os/exec"
	"strconv"
)

func configureDaemonCommand(_ *exec.Cmd) {}

func stopDaemonProcess(process *os.Process, pid int) error {
	// taskkill /T terminates the full descendant tree; Process.Kill is the
	// portable fallback when taskkill is unavailable or the tree already moved.
	if err := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run(); err == nil {
		return nil
	}
	return process.Kill()
}
