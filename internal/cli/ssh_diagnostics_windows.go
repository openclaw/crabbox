package cli

import (
	"io"
	"os/exec"
)

func runSSHCommandWithLocalDiagnostics(cmd *exec.Cmd, stdout, stderr io.Writer) (bool, error) {
	return false, runSSHCommand(cmd, stdout, stderr)
}
