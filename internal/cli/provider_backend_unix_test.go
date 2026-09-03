//go:build !windows

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecCommandRunnerBoundsInheritedOutputPipes(t *testing.T) {
	t.Setenv(controllerProcessTreeOwnedEnv, "")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pidPath := dir + "/child.pid"
	ctx, cancel := context.WithCancel(t.Context())
	type commandOutcome struct {
		result LocalCommandResult
		err    error
	}
	done := make(chan commandOutcome, 1)
	returned := false
	t.Cleanup(func() {
		cancel()
		if data, err := os.ReadFile(pidPath); err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil || pid <= 0 {
				t.Errorf("invalid helper child pid %q", data)
			} else {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		} else if !os.IsNotExist(err) {
			t.Errorf("read helper child pid: %v", err)
		}
		if !returned {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("captured command did not return after helper cleanup")
			}
		}
	})
	go func() {
		result, err := (execCommandRunner{}).Run(ctx, LocalCommandRequest{
			Name: executable,
			Args: []string{"-test.run=^TestExecCommandRunnerOutputLimitHelperProcess$", "--"},
			Env: []string{
				"HOME=" + dir,
				"CRABBOX_TEST_OUTPUT_LIMIT_HELPER=retained-pipe",
				"CRABBOX_TEST_OUTPUT_LIMIT_CHILD_PID=" + pidPath,
			},
			MaxCapturedOutputBytes: 1024,
		})
		done <- commandOutcome{result: result, err: err}
	}()
	select {
	case outcome := <-done:
		returned = true
		if !errors.Is(outcome.err, exec.ErrWaitDelay) {
			t.Fatalf("retained output pipe error=%v stdout=%q stderr=%q", outcome.err, outcome.result.Stdout, outcome.result.Stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("captured command waited for the descendant's inherited output pipes")
	}
}

func TestControllerOwnedCapturedCommandKeepsOuterProcessGroup(t *testing.T) {
	t.Setenv(controllerProcessTreeOwnedEnv, "1")
	result, err := (execCommandRunner{}).Run(context.Background(), LocalCommandRequest{
		Name:                   "sh",
		Args:                   []string{"-c", "ps -o pgid= -p $$; test -z \"$" + controllerProcessTreeOwnedEnv + "\""},
		Env:                    os.Environ(),
		MaxCapturedOutputBytes: 1024,
	})
	if err != nil {
		t.Fatalf("controller-owned provider command: %v stderr=%q", err, result.Stderr)
	}
	processGroup, err := strconv.Atoi(strings.TrimSpace(result.Stdout))
	if err != nil {
		t.Fatalf("provider process group %q: %v", result.Stdout, err)
	}
	if processGroup != syscall.Getpgrp() {
		t.Fatalf("provider process group=%d outer=%d", processGroup, syscall.Getpgrp())
	}
}
