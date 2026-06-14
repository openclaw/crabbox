//go:build darwin

package cli

import (
	"os/exec"
	"syscall"
)

func configureControllerCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
