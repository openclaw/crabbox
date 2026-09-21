//go:build darwin

package lume

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
)

func systemForeignVMUse(path string) (string, error) {
	cmd := exec.Command("/usr/sbin/lsof", "-F", "p", "+D", path)
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	output, err := cmd.Output()
	exitCode := 0
	var exitErr *exec.ExitError
	if err != nil {
		if !errors.As(err, &exitErr) {
			return "", err
		}
		exitCode = exitErr.ExitCode()
	}
	return lsofVMUseResult(string(output), diagnostics.String(), exitCode, os.Getpid())
}
