//go:build !windows

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestControllerRunContextCancellationKillsProcessGroup(t *testing.T) {
	runner, pidPath, heartbeatPath := controllerProcessTreeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	err := runner.runWithStarted(ctx, controllerWorkspaceRequest{ID: "cancel-tree"}, []string{"warmup"}, io.Discard, func() error {
		_ = waitForPIDFile(t, pidPath)
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v want context canceled", err)
	}
	assertControllerProcessTreeStopped(t, waitForPIDFile(t, pidPath), heartbeatPath)
}

func TestControllerRunStartedCallbackFailureKillsProcessGroup(t *testing.T) {
	runner, pidPath, heartbeatPath := controllerProcessTreeFixture(t)
	sentinel := errors.New("state persistence failed")
	err := runner.runWithStarted(context.Background(), controllerWorkspaceRequest{ID: "callback-tree"}, []string{"warmup"}, io.Discard, func() error {
		_ = waitForPIDFile(t, pidPath)
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error=%v want callback failure", err)
	}
	assertControllerProcessTreeStopped(t, waitForPIDFile(t, pidPath), heartbeatPath)
}

func TestControllerProviderIdentityCancellationKillsProcessTree(t *testing.T) {
	runner, pidPath, heartbeatPath := controllerProcessTreeFixture(t)
	runner.opts.StateFile = filepath.Join(filepath.Dir(pidPath), "controller-state.json")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := runner.ProviderIdentity(ctx)
		result <- err
	}()
	pid := waitForPIDFile(t, pidPath)
	cancel()
	err := <-result
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("provider identity error=%v", err)
	}
	assertControllerProcessTreeStopped(t, pid, heartbeatPath)
	entries, readErr := os.ReadDir(controllerChildStateDirectory(runner.opts.StateFile))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("provider identity child handles remain after reap: %v provider error=%v", entries, err)
	}
}

func TestControllerProcessGroupExitWaitsThroughTransientStopError(t *testing.T) {
	checks := 0
	err := waitForControllerProcessGroupExit(42, syscall.EPERM, func(processGroupID int) bool {
		if processGroupID != 42 {
			t.Fatalf("process group id=%d", processGroupID)
		}
		checks++
		return checks == 1
	}, time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("transient stop error was retained after group exit: %v", err)
	}
	if checks != 2 {
		t.Fatalf("liveness checks=%d want 2", checks)
	}
}

func TestControllerProcessGroupExitRetainsPersistentStopError(t *testing.T) {
	err := waitForControllerProcessGroupExit(42, syscall.EPERM, func(int) bool { return true }, time.Now().Add(-time.Second))
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("error=%v want EPERM", err)
	}
	if !strings.Contains(err.Error(), "controller process group 42 survived termination") {
		t.Fatalf("error=%v", err)
	}
}

func controllerProcessTreeFixture(t *testing.T) (*execControllerWorkspaceRunner, string, string) {
	t.Helper()
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "child.pid")
	heartbeatPath := filepath.Join(dir, "heartbeat")
	binary := filepath.Join(dir, "crabbox-fixture")
	script := "#!/bin/sh\n(while :; do printf .; sleep 0.02; done) >>\"$CONTROLLER_HEARTBEAT\" &\nchild=$!\nprintf '%s\\n' \"$child\" >\"$CONTROLLER_CHILD_PID\"\nwait \"$child\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROLLER_CHILD_PID", pidPath)
	t.Setenv("CONTROLLER_HEARTBEAT", heartbeatPath)
	return &execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Binary: binary}}, pidPath, heartbeatPath
}

func assertControllerProcessTreeStopped(t *testing.T, pid int, heartbeatPath string) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	before, err := os.Stat(heartbeatPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	after, err := os.Stat(heartbeatPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var beforeSize, afterSize int64
	if before != nil {
		beforeSize = before.Size()
	}
	if after != nil {
		afterSize = after.Size()
	}
	if afterSize != beforeSize {
		t.Fatalf("controller descendant %d kept running after process-group termination: heartbeat grew from %d to %d bytes", pid, beforeSize, afterSize)
	}
}
