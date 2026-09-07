//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func configureWebVNCDaemonChildCancellation(cmd *exec.Cmd) {
	// Signal only this unreaped child. Group signaling would also hit the
	// supervisor, and a second TERM can interrupt the child's deferred cleanup.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
}

func stopWebVNCDaemonProcessTree(identity webVNCDaemonIdentity, path string) error {
	deadline := time.Now().Add(5 * time.Second)
	// Leave escalation inside the existing five-second stop budget, and before
	// the supervisor's child WaitDelay can force an unconfirmed child exit.
	grace := time.Now().Add(3 * time.Second)
	if webVNCDaemonProcessGroupAlive(identity.PID) {
		if err := signalWebVNCDaemonSupervisor(identity, syscall.SIGTERM); err != nil {
			return err
		}
	}
	for webVNCDaemonProcessGroupAlive(identity.PID) {
		if time.Now().After(grace) {
			if err := signalWebVNCDaemonSupervisor(identity, syscall.SIGKILL); err != nil {
				return fmt.Errorf("WebVNC daemon graceful cleanup timed out: %w", err)
			}
			for webVNCDaemonProcessGroupAlive(identity.PID) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			return errors.Join(fmt.Errorf("WebVNC daemon graceful cleanup timed out"), errWebVNCDaemonCleanupUnconfirmed)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !webVNCDaemonCleanupConfirmed(identity, path) {
		return errWebVNCDaemonCleanupUnconfirmed
	}
	return nil
}

func signalWebVNCDaemonSupervisor(identity webVNCDaemonIdentity, signal syscall.Signal) error {
	command, alive := webVNCDaemonProcessCommand(identity.PID)
	started, err := webVNCDaemonProcessStartIdentity(identity.PID)
	if !alive || err != nil || !webVNCDaemonIdentityMatchesProcess(identity, command, started) {
		return fmt.Errorf("refusing to signal WebVNC daemon pid %d without its recorded supervisor identity", identity.PID)
	}
	if signal == syscall.SIGKILL {
		pgid, err := syscall.Getpgid(identity.PID)
		if err != nil || pgid != identity.PID {
			return fmt.Errorf("refusing to signal WebVNC daemon group %d without its recorded leader", identity.PID)
		}
		return syscall.Kill(-identity.PID, signal)
	}
	return syscall.Kill(identity.PID, signal)
}

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
