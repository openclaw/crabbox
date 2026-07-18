//go:build darwin

package lume

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func systemForeignVMUse(path string) (string, error) {
	output, err := exec.Command("/usr/sbin/lsof", "-F", "p", "+D", path).CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(strings.TrimSpace(string(output))) == 0 {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	self := "p" + strconv.Itoa(os.Getpid())
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "p") && len(line) > 1 && line != self {
			return fmt.Sprintf("process %s", line[1:]), nil
		}
	}
	return "", nil
}
