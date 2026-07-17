package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunPondMeshForwardsGracefulCancelReturnsNoError proves that a clean,
// user-initiated shutdown (ctx cancellation, i.e. Ctrl-C on `crabbox pond
// connect`) is NOT reported as a tunnel failure — even when the child is an
// ssh that CATCHES SIGINT and exits with a non-zero status.
//
// This is the regression for hole (1): v2 tore forwards down with a graceful,
// catchable SIGINT. Real ssh (and this fake) traps SIGINT and exits non-zero,
// so its terminal state is ProcessState.Exited() with a real code — which v2's
// classifier reads as a GENUINE failure (not Signaled()), and a clean Ctrl-C
// therefore returns a spurious tunnel error. v3 removes the graceful signal and
// relies on exec.CommandContext's DEFAULT cancellation (SIGKILL on Unix), which
// is uncatchable: the child's trap never fires, its terminal state is always
// Signaled(), and WasTerminatedByOurCancel suppresses it. So this test FAILS on
// v2 (spurious exit-255 error) and PASSES on v3.
//
// It drives the REAL production entry point runPondMeshForwards with the real
// pondMeshExecRunner (opts.Runner == nil falls back to pondMeshDefaultRunner)
// so the actual ProcessState logic is exercised — no handle is mocked. The only
// substitution is a fake `ssh` binary injected via PATH so no network is
// touched.
func TestRunPondMeshForwardsGracefulCancelReturnsNoError(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("fake ssh shell script requires a unix-like OS")
	}

	// Fake ssh that holds the "tunnel" open like a healthy `ssh -N -L`, but
	// TRAPS SIGINT/SIGTERM and exits 255 — exactly what real ssh does on a
	// catchable signal. Under v3's SIGKILL teardown the trap never fires.
	binDir := t.TempDir()
	fakeSSH := filepath.Join(binDir, "ssh")
	script := "#!/bin/sh\ntrap 'exit 255' INT TERM\nwhile :; do sleep 0.05; done\n"
	if err := os.WriteFile(fakeSSH, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	members := []pondMember{{
		Name:  "peer-a",
		Lease: "lease-a",
		SSH: SSHTarget{
			User:                   "crab",
			Host:                   "127.0.0.1",
			Port:                   "22",
			DisableHostKeyChecking: true,
			NoControlMaster:        true,
		},
	}}
	summary := pondMeshSummary{Forwards: []pondMeshForward{{
		Peer:       "peer-a",
		RemotePort: 8080,
		LocalPort:  18080,
		LeaseID:    "lease-a",
	}}}
	// Runner deliberately nil: exercise the production pondMeshExecRunner.
	opts := pondConnectOptions{Stdout: io.Discard, Stderr: io.Discard}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- runPondMeshForwards(ctx, opts, members, summary) }()

	// Give the fake tunnel time to start, then simulate Ctrl-C.
	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("graceful ctx cancellation must not be reported as a tunnel failure, got: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runPondMeshForwards did not return after ctx cancellation")
	}
}
