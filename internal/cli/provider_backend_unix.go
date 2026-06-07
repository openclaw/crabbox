//go:build !windows

package cli

import (
	"os/exec"
	"syscall"
)

func configureLocalCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killLocalCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return cmd.Process.Kill()
	}
	return nil
}
