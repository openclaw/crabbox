//go:build !windows

package cli

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStopDaemonProcessKillsProcessGroup(t *testing.T) {
	pidPath := t.TempDir() + "/child.pid"
	cmd := exec.Command("sh", "-c", "sleep 30 & echo $! >"+shellQuote(pidPath)+"; wait")
	configureDaemonCommand(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stopDaemonProcess(cmd.Process, cmd.Process.Pid)
		_, _ = cmd.Process.Wait()
	})

	childPID := waitForPIDFile(t, pidPath)
	if err := stopDaemonProcess(cmd.Process, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := syscall.Kill(childPID, 0)
		if err == syscall.ESRCH {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %d still alive after daemon stop; last kill probe err=%v", childPID, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	// The child fork/exec that writes this file can be slow under CI load, so
	// allow generous slack before declaring the descendant never started.
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			value := strings.TrimSpace(string(data))
			if value != "" {
				pid, parseErr := strconv.Atoi(value)
				if parseErr == nil {
					return pid
				}
				lastErr = parseErr
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for pid file %s: %v", path, lastErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
