//go:build !windows

package cli

import (
	"os/exec"
	"strconv"
	"strings"
)

func webVNCDaemonProcessCommand(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	out, err := exec.Command("ps", "-ww", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return "", false
	}
	command := strings.TrimSpace(string(out))
	return command, command != ""
}
