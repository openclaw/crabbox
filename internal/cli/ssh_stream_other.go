//go:build !windows

package cli

import (
	"io"
	"os/exec"
)

func runCommandWithPlatformStreams(cmd *exec.Cmd, stdout, stderr io.Writer) error {
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
