//go:build !windows

package asciibox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestReleaseLeaseBoundsNativeChildAndRetainsClaim(t *testing.T) {
	b, _, claim, lease := ownedFixture(t)
	cliPath := filepath.Join(t.TempDir(), "box")
	if err := os.WriteFile(cliPath, []byte("#!/bin/sh\nsleep 30 &\nprintf 'ready\\n'\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	b.rt.Stderr = &output
	ready := make(boxProgressOutput, 1)
	runner := core.RuntimeForProviderOperation(&output).Exec
	withFakeAPI(t, &client{cliPath: cliPath, runner: boxCommandRunnerFunc(func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		req.Stdout = ready
		return runner.Run(ctx, req)
	})})
	// Native process waits cannot advance synctest time. Expire a controlled
	// caller deadline only after the real shell has spawned its child.
	ctx := &boxNativeDeadlineContext{Context: context.Background(), deadline: time.Now().Add(100 * time.Millisecond), done: make(chan struct{})}
	var expireOnce sync.Once
	expire := func() { expireOnce.Do(func() { close(ctx.done) }) }
	defer expire()
	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- b.ReleaseLease(ctx, core.ReleaseLeaseRequest{Lease: lease}) }()
	guard := time.NewTimer(5 * time.Second)
	defer guard.Stop()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("native command returned before child readiness: %v", err)
	case <-guard.C:
		t.Fatal("native child did not signal readiness")
	}
	expire()
	var err error
	select {
	case err = <-done:
	case <-guard.C:
		t.Fatal("native child outlived bounded cleanup")
	}
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "phase=ownership-check") {
		t.Fatalf("lost cleanup deadline/phase: %v", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("native child outlived bounded cleanup")
	}
	if !strings.Contains(output.String(), "phase=ownership-check") || !strings.Contains(output.String(), "remaining=") {
		t.Fatalf("missing native progress: %s", output.String())
	}
	assertClaimRetained(t, claim)
}

type boxNativeDeadlineContext struct {
	context.Context
	deadline time.Time
	done     chan struct{}
}

func (c *boxNativeDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *boxNativeDeadlineContext) Done() <-chan struct{}       { return c.done }
func (c *boxNativeDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
