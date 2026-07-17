//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func terminateWebVNCDaemonProcessTree(processGroupID int) error {
	if processGroupID <= 0 {
		return syscall.EINVAL
	}
	if err := syscall.Kill(-processGroupID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		if !webVNCDaemonProcessGroupAlive(processGroupID) {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for webVNCDaemonProcessGroupAlive(processGroupID) {
		if time.Now().After(deadline) {
			return fmt.Errorf("WebVNC daemon process group %d survived termination", processGroupID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func webVNCDaemonProcessGroupAlive(processGroupID int) bool {
	if processGroupID <= 0 {
		return false
	}
	if output, err := systemInspectionCommand("ps", "-axo", "pgid=,stat=").Output(); err == nil {
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			pgid, err := strconv.Atoi(fields[0])
			if err == nil && pgid == processGroupID && !strings.HasPrefix(strings.ToUpper(fields[1]), "Z") {
				return true
			}
		}
		return false
	}
	err := syscall.Kill(-processGroupID, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
