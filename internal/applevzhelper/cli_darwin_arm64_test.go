//go:build darwin && arm64 && cgo

package applevzhelper

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunStartCleansUpDaemonAndInstanceDirectoryOnReadinessTimeout(t *testing.T) {
	stateRoot := t.TempDir()
	name := "timeout-cleanup"
	pidFile := filepath.Join(t.TempDir(), "helper.pid")
	helperPath := filepath.Join(t.TempDir(), "fake-helper")
	helperScript := `#!/bin/sh
printf '%s\n' "$$" > "$CRABBOX_TEST_HELPER_PID_FILE"
trap 'exit 0' TERM INT
while :; do sleep 1; done
`
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o755); err != nil {
		t.Fatalf("write fake helper: %v", err)
	}
	t.Setenv("CRABBOX_TEST_HELPER_PID_FILE", pidFile)

	originalPrepare := prepareInstanceAssetsFunc
	originalExecutable := helperExecutable
	originalProcessStartTime := processStartTime
	originalReadyTimeout := runStartReadyTimeout
	originalStartPoll := runStartPollInterval
	originalTerminateGrace := terminateInstanceGraceTime
	originalTerminatePoll := terminateInstancePollTime
	t.Cleanup(func() {
		prepareInstanceAssetsFunc = originalPrepare
		helperExecutable = originalExecutable
		processStartTime = originalProcessStartTime
		runStartReadyTimeout = originalReadyTimeout
		runStartPollInterval = originalStartPoll
		terminateInstanceGraceTime = originalTerminateGrace
		terminateInstancePollTime = originalTerminatePoll
	})

	prepareInstanceAssetsFunc = func(_ context.Context, cfg startConfig) (Instance, error) {
		inst := cfg.Instance
		inst.SourceImage = cfg.Instance.Image
		inst.DiskPath = DiskPath(cfg.StateRoot, inst.Name)
		inst.SeedPath = SeedPath(cfg.StateRoot, inst.Name)
		inst.EFIVariableStorePath = EFIPath(cfg.StateRoot, inst.Name)
		inst.ConsoleLogPath = ConsoleLogPath(cfg.StateRoot, inst.Name)
		for _, path := range []string{inst.DiskPath, inst.SeedPath, inst.EFIVariableStorePath, inst.ConsoleLogPath} {
			if err := os.WriteFile(path, []byte("test asset\n"), 0o644); err != nil {
				return Instance{}, err
			}
		}
		return inst, nil
	}
	helperExecutable = func() (string, error) { return helperPath, nil }
	processStartTime = func(pid int) (string, error) { return strconv.Itoa(pid) + "-start", nil }
	runStartReadyTimeout = time.Second
	runStartPollInterval = 5 * time.Millisecond
	terminateInstanceGraceTime = 500 * time.Millisecond
	terminateInstancePollTime = 5 * time.Millisecond

	err := runStart([]string{
		"--state-root", stateRoot,
		"--name", name,
		"--lease-id", "lease-test",
		"--slug", "my-app",
		"--image", "test.img",
		"--image-sha256", "",
		"--ssh-user", "alice",
		"--ssh-public-key", "ssh-ed25519 AAAATEST alice@example.com",
		"--work-root", "/workspace",
		"--cpus", "2",
		"--memory-mib", "2048",
		"--disk-gib", "16",
	}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for helper daemon to report readiness") {
		t.Fatalf("runStart error = %v, want readiness timeout", err)
	}

	pidData, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("read helper pid: %v", readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if parseErr != nil {
		t.Fatalf("parse helper pid %q: %v", string(pidData), parseErr)
	}
	if err := waitForDeadPID(pid, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(InstanceDir(stateRoot, name)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("instance directory stat error = %v, want os.ErrNotExist", statErr)
	}
}

func TestTerminateInstanceSkipsSignalWhenPIDIdentityMismatches(t *testing.T) {
	root := t.TempDir()
	name := "stale-pid"
	mustCreateInstanceDir(t, root, name)
	cmd := startSleepProcess(t)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	originalProcessStartTime := processStartTime
	t.Cleanup(func() { processStartTime = originalProcessStartTime })
	processStartTime = func(pid int) (string, error) {
		if pid != cmd.Process.Pid {
			t.Fatalf("processStartTime pid=%d want %d", pid, cmd.Process.Pid)
		}
		return "actual-start", nil
	}

	err := terminateInstance(root, name, Instance{PID: cmd.Process.Pid, PIDStartedAt: "old-start"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(InstanceDir(root, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instance directory stat error=%v, want os.ErrNotExist", err)
	}
	if !pidAlive(cmd.Process.Pid) {
		t.Fatal("identity mismatch should not signal the live process")
	}
}

func TestTerminateInstanceSignalsOnlyMatchingPIDIdentity(t *testing.T) {
	root := t.TempDir()
	name := "matching-pid"
	mustCreateInstanceDir(t, root, name)
	cmd := startSleepProcess(t)
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	originalProcessStartTime := processStartTime
	originalTerminateGrace := terminateInstanceGraceTime
	originalTerminatePoll := terminateInstancePollTime
	t.Cleanup(func() {
		processStartTime = originalProcessStartTime
		terminateInstanceGraceTime = originalTerminateGrace
		terminateInstancePollTime = originalTerminatePoll
	})
	processStartTime = func(pid int) (string, error) {
		if pid != cmd.Process.Pid {
			t.Fatalf("processStartTime pid=%d want %d", pid, cmd.Process.Pid)
		}
		return "matching-start", nil
	}
	terminateInstanceGraceTime = time.Second
	terminateInstancePollTime = 5 * time.Millisecond

	err := terminateInstance(root, name, Instance{PID: cmd.Process.Pid, PIDStartedAt: "matching-start"})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitForProcessExit(cmd, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	waited = true
	if _, err := os.Stat(InstanceDir(root, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instance directory stat error=%v, want os.ErrNotExist", err)
	}
}

func TestHandleStartReadinessMetadataStoppedBeforeReadiness(t *testing.T) {
	root := t.TempDir()
	name := "stopped-before-ready"
	pid := os.Getpid()
	mustCreateInstanceDir(t, root, name)

	handled, err := handleStartReadinessMetadata(root, name, Instance{
		Name:      name,
		Status:    StatusStopped,
		PID:       pid,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}, pid, nil, &bytes.Buffer{})

	if !handled {
		t.Fatal("expected stopped status to be handled")
	}
	if err == nil {
		t.Fatal("expected stopped-before-readiness error")
	}
	if got := err.Error(); !strings.Contains(got, "apple-vz helper stopped before reporting readiness (status=stopped)") {
		t.Fatalf("expected stopped-before-readiness error, got %q", got)
	}
	if strings.Contains(err.Error(), "helper daemon exited before the VM reached running state") {
		t.Fatalf("expected specific stopped error, got misleading daemon-exited error: %v", err)
	}
	if _, err := os.Stat(InstanceDir(root, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected startup instance directory to be removed, stat err=%v", err)
	}
}

func TestHandleStartReadinessMetadataStoppingDeadPIDCleansInstanceDir(t *testing.T) {
	root := t.TempDir()
	name := "stopping-dead"
	pid := unusedPID(t)
	mustCreateInstanceDir(t, root, name)

	handled, err := handleStartReadinessMetadata(root, name, Instance{
		Name:      name,
		Status:    StatusStopping,
		PID:       pid,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}, pid, nil, &bytes.Buffer{})

	if !handled {
		t.Fatal("expected stopping status with a dead PID to be handled")
	}
	if err == nil {
		t.Fatal("expected stopped-before-readiness error")
	}
	if got := err.Error(); !strings.Contains(got, "apple-vz helper stopped before reporting readiness (status=stopped)") {
		t.Fatalf("expected normalized stopped-before-readiness error, got %q", got)
	}
	if _, err := os.Stat(InstanceDir(root, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected startup instance directory to be removed, stat err=%v", err)
	}
}

func waitForDeadPID(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return errors.New("helper process remained alive after runStart timeout cleanup")
}

func waitForProcessExit(cmd *exec.Cmd, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return errors.New("helper process remained alive after matching identity cleanup")
	case <-done:
		return nil
	}
}

func startSleepProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep process: %v", err)
	}
	return cmd
}

func mustCreateInstanceDir(t *testing.T, root, name string) {
	t.Helper()
	if err := os.MkdirAll(InstanceDir(root, name), 0o755); err != nil {
		t.Fatalf("create instance dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(InstanceDir(root, name), "sentinel"), []byte("created by start\n"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
}

func unusedPID(t *testing.T) int {
	t.Helper()
	for pid := 999999; pid > 100000; pid-- {
		if !pidAlive(pid) {
			return pid
		}
	}
	t.Fatal("failed to find an unused pid")
	return 0
}
