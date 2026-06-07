//go:build windows

package cli

import "os/exec"

func configureLocalCommand(cmd *exec.Cmd) {}

func killLocalCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
