//go:build !windows

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRunPondMeshForwardsGenuineExitSurvivesSiblingCancel is the regression for
// the race steipete flagged as "not safe to land": a genuine per-member-process
// failure must be surfaced even while a SIBLING member is being torn down —
// classifying by anything shared (the ctx, or a "some cancel happened" flag)
// discards it.
//
// Two forwards run concurrently against the REAL production runner:
//   - forward A exits 7 ON ITS OWN — a genuine, peer-attributable non-zero
//     status. The OS records ProcessState.Exited() (a real code), never
//     Signaled(), and because A exited on its own the ctx watchdog never fires
//     A's Cancel hook, so A's `cancelled` flag stays false. Both facts make
//     WasTerminatedByOurCancel return false, so exit 7 is recorded.
//   - forward B is a healthy long-running tunnel. A's exit triggers cancel(),
//     the ctx watchdog SIGKILLs B, and B's terminal state is Signaled() with
//     `cancelled` true — WasTerminatedByOurCancel returns true, so B's
//     "signal: killed" is SUPPRESSED and never masks A's genuine exit 7.
//
// This is the v3 shape: v3 tears down with an UNCATCHABLE SIGKILL (not a
// graceful SIGINT), so a forward can only reach a genuine Exited() code by
// exiting on its own — which is exactly what makes the Exited-vs-Signaled
// provenance split unambiguous. A classifier that suppressed every error once
// any cancel occurred would drop A's exit 7 and return nil; the per-forward
// process provenance redesign returns the genuine *exec.ExitError.
func TestRunPondMeshForwardsGenuineExitSurvivesSiblingCancel(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("fake ssh shell script requires a unix-like OS")
	}

	const (
		portGenuine = 19001 // forward A: exits 7 on its own (Exited(7))
		portCancel  = 19002 // forward B: healthy tunnel, SIGKILLed on teardown
	)

	binDir := t.TempDir()
	pidDir := t.TempDir()
	// One fake ssh binary that branches on the -L local port. Forward A exits 7
	// on its own — a genuine non-zero code the OS reports as Exited(), not
	// Signaled(). It first waits for Forward B to record its PID, so B is up
	// (and its PID file written) before A's exit triggers the teardown SIGKILL;
	// otherwise a slow runner can kill B before its shell writes the PID and the
	// leak check below has nothing to read. Forward B holds a healthy tunnel open
	// via `exec sleep` (so the tracked PID IS the sleep and our SIGKILL reaps it
	// directly, leaving no orphan). Each records its PID so we can prove the
	// children are reaped (no waiter/child leak).
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *:" + strconv.Itoa(portGenuine) + ":*)\n" +
		"    echo $$ > \"$CBX_TEST_PIDDIR/genuine\"\n" +
		"    i=0; while [ ! -s \"$CBX_TEST_PIDDIR/cancel\" ] && [ $i -lt 500 ]; do i=$((i+1)); sleep 0.01; done\n" +
		"    exit 7 ;;\n" +
		"  *:" + strconv.Itoa(portCancel) + ":*)\n" +
		"    echo $$ > \"$CBX_TEST_PIDDIR/cancel\"\n" +
		"    exec sleep 60 ;;\n" +
		"esac\n"
	fakeSSH := filepath.Join(binDir, "ssh")
	if err := os.WriteFile(fakeSSH, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CBX_TEST_PIDDIR", pidDir)

	members := []pondMember{
		{
			Name:  "peer-genuine",
			Lease: "lease-genuine",
			SSH:   SSHTarget{User: "crab", Host: "127.0.0.1", Port: "22", DisableHostKeyChecking: true, NoControlMaster: true},
		},
		{
			Name:  "peer-cancel",
			Lease: "lease-cancel",
			SSH:   SSHTarget{User: "crab", Host: "127.0.0.1", Port: "22", DisableHostKeyChecking: true, NoControlMaster: true},
		},
	}
	summary := pondMeshSummary{Forwards: []pondMeshForward{
		{Peer: "peer-genuine", RemotePort: 8081, LocalPort: portGenuine, LeaseID: "lease-genuine"},
		{Peer: "peer-cancel", RemotePort: 8082, LocalPort: portCancel, LeaseID: "lease-cancel"},
	}}
	// Runner deliberately nil: exercise the production pondMeshExecRunner.
	opts := pondConnectOptions{Stdout: io.Discard, Stderr: io.Discard}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- runPondMeshForwards(ctx, opts, members, summary) }()

	var err error
	select {
	case err = <-errCh:
	case <-time.After(15 * time.Second):
		t.Fatal("runPondMeshForwards did not return")
	}

	if err == nil {
		t.Fatal("genuine forward failure (exit 7) was swallowed after a sibling forward cancelled the context; want a non-nil error")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected the genuine failure to surface as *exec.ExitError, got %T: %v", err, err)
	}
	if code := exitErr.ExitCode(); code != 7 {
		t.Fatalf("expected genuine exit code 7 to survive the sibling cancel, got %d (%v)", code, err)
	}

	// Leak proof: both fake-ssh children must be reaped (their waiter
	// goroutines ran Wait to completion — no goroutine or child process leaks
	// survive teardown). runPondMeshForwards only returns after wg.Wait(), so
	// by here every waiter has exited; assert the PIDs are gone.
	for _, name := range []string{"genuine", "cancel"} {
		pid := readPIDFile(t, filepath.Join(pidDir, name))
		if alive := processAlive(pid); alive {
			t.Fatalf("fake-ssh child for %q (pid %d) still alive after teardown: waiter/child leak", name, pid)
		}
	}
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	// The child writes its PID asynchronously; allow a brief window.
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid file %s never became readable", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// processAlive reports whether a process with the given PID is still running,
// using signal 0 (existence probe). A reaped child returns ESRCH.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
