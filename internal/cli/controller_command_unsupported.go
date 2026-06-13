//go:build !darwin && !linux

package cli

import "os/exec"

func configureControllerCommand(cmd *exec.Cmd) {
	configureDaemonCommand(cmd)
}
