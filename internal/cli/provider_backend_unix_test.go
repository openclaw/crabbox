//go:build !windows

package cli

import (
	"context"
	"errors"
	"io"
	"syscall"
	"testing"
	"time"
)

func TestExecCommandRunnerKillsProcessGroupOnContextCancel(t *testing.T) {
	pidPath := t.TempDir() + "/child.pid"
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := (execCommandRunner{}).Run(ctx, LocalCommandRequest{
		Name:   "sh",
		Args:   []string{"-c", "sleep 30 & echo $! >" + shellQuote(pidPath) + "; wait"},
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run err=%v, want context deadline", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Run elapsed=%s, want prompt context cancellation", elapsed)
	}

	childPID := waitForPIDFile(t, pidPath)
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Kill(childPID, 0)
		if err == syscall.ESRCH {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %d still alive after context cancellation; last kill probe err=%v", childPID, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
