//go:build darwin || linux

package cli

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func stopControllerProcessGroup(processGroupID int) error {
	if processGroupID <= 0 {
		return syscall.EINVAL
	}
	if err := syscall.Kill(-processGroupID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

func controllerProcessGroupAlive(processGroupID int) bool {
	if processGroupID <= 0 {
		return false
	}
	if output, err := exec.Command("ps", "-axo", "pgid=,stat=").Output(); err == nil {
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			pgid, err := strconv.Atoi(fields[0])
			if err != nil || pgid != processGroupID {
				continue
			}
			if !strings.HasPrefix(strings.ToUpper(fields[1]), "Z") {
				return true
			}
		}
		return false
	}
	err := syscall.Kill(-processGroupID, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
