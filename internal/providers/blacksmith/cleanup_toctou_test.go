package blacksmith

// Repro for a cross-process TOCTOU in cleanupFailedWarmup (backend.go):
// the failed-warmup cleanup decides which testboxes to stop from a list-diff
// snapshot (listIDsBestEffort before the warmup command) plus a config-only
// matcher (blacksmithMatchesConfig: workflow/job/ref, no per-invocation
// ownership marker). A testbox created concurrently by ANOTHER crabbox
// process with the same config (e.g. two CI jobs of the same repo) appears
// after the snapshot, matches the config, and is stopped — severing that
// other process's healthy, in-use lease.
//
// The test models two crabbox processes (backend A and backend B) sharing one
// fake Blacksmith control plane. The interleaving is sequenced
// deterministically:
//   1. A's warmupLease takes its "before" snapshot (control plane empty).
//   2. While A's `blacksmith testbox warmup` command is in flight, process B
//      runs its own warmup for the same config; it succeeds, creating and
//      claiming tbx_bee123 (simulated inside A's warmup command handler).
//   3. A's warmup command fails WITHOUT printing a tbx_ id (queue error).
//   4. A's cleanupFailedWarmup lists --all, sees tbx_bee123 (absent from A's
//      before-map, matches workflow/job/ref) and stops it.
//
// Correct behavior: A must not stop a testbox it did not create, so
// tbx_bee123 must survive with its lease claim intact. Current code stops it,
// so this test FAILS — that failure is the bug proof.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// crossProcControlPlane is a shared fake Blacksmith control plane visible to
// both simulated crabbox processes.
type crossProcControlPlane struct {
	mu      sync.Mutex
	boxes   map[string]bool
	stopped []string
}

func (cp *crossProcControlPlane) add(id string) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.boxes[id] = true
}

func (cp *crossProcControlPlane) stop(id string) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.boxes[id] {
		delete(cp.boxes, id)
		cp.stopped = append(cp.stopped, id)
	}
}

func (cp *crossProcControlPlane) has(id string) bool {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.boxes[id]
}

func (cp *crossProcControlPlane) stoppedIDs() []string {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	out := append([]string(nil), cp.stopped...)
	sort.Strings(out)
	return out
}

func (cp *crossProcControlPlane) listOutput() string {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	var b strings.Builder
	b.WriteString("ID STATUS REPO WORKFLOW JOB REF CREATED\n")
	ids := make([]string, 0, len(cp.boxes))
	for id := range cp.boxes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Fprintf(&b, "%s running openclaw .github/workflows/testbox.yml check main 2026-07-14T10:00:00.000000Z\n", id)
	}
	return b.String()
}

// crossProcRunner is a per-process fake `blacksmith` CLI backed by the shared
// control plane.
type crossProcRunner struct {
	cp       *crossProcControlPlane
	onWarmup func(LocalCommandRequest) (LocalCommandResult, error)
}

func (r *crossProcRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	args := req.Args
	if len(args) >= 2 && args[0] == "testbox" {
		switch args[1] {
		case "list":
			return LocalCommandResult{Stdout: r.cp.listOutput()}, nil
		case "warmup":
			return r.onWarmup(req)
		case "stop":
			for i, arg := range args {
				if arg == "--id" && i+1 < len(args) {
					r.cp.stop(args[i+1])
				}
			}
			return LocalCommandResult{}, nil
		}
	}
	return LocalCommandResult{}, nil
}

func TestCrossProcCleanupFailedWarmupStopsConcurrentProcessTestbox(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	originalDelay := blacksmithCleanupDelay
	originalAttempts := blacksmithCleanupAttempts
	originalQuiet := blacksmithCleanupQuiet
	blacksmithCleanupDelay = time.Millisecond
	blacksmithCleanupAttempts = 3
	blacksmithCleanupQuiet = 1
	t.Cleanup(func() {
		blacksmithCleanupDelay = originalDelay
		blacksmithCleanupAttempts = originalAttempts
		blacksmithCleanupQuiet = originalQuiet
	})

	// Both processes run the same repo/config — two CI jobs of one project.
	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	cfg.Blacksmith.Job = "check"
	cfg.Blacksmith.Ref = "main"

	cp := &crossProcControlPlane{boxes: map[string]bool{}}

	// Process B: its warmup succeeds and creates tbx_bee123.
	runnerB := &crossProcRunner{cp: cp, onWarmup: func(LocalCommandRequest) (LocalCommandResult, error) {
		cp.add("tbx_bee123")
		return LocalCommandResult{Stdout: "queued tbx_bee123\n"}, nil
	}}
	backendB := newTestBlacksmithBackend(cfg, runnerB)

	// Process A: its warmup fails with a queue error that prints no tbx_ id.
	// While A's warmup command is in flight (strictly AFTER A took its
	// "before" list snapshot), process B completes its own warmup and claims
	// its healthy testbox — the concurrent interleaving under test.
	leaseB := ""
	runnerA := &crossProcRunner{cp: cp}
	runnerA.onWarmup = func(LocalCommandRequest) (LocalCommandResult, error) {
		id, _, err := backendB.warmupLease(context.Background(), Repo{Root: "/repo-b"}, false, "")
		if err != nil {
			t.Fatalf("process B warmup failed: %v", err)
		}
		leaseB = id
		return LocalCommandResult{ExitCode: 1, Stdout: "error: delegated queue unavailable\n"}, errors.New("exit status 1")
	}
	backendA := newTestBlacksmithBackend(cfg, runnerA)

	_, _, err := backendA.warmupLease(context.Background(), Repo{Root: "/repo-a"}, false, "")
	if err == nil {
		t.Fatal("expected process A warmup failure")
	}
	if leaseB != "tbx_bee123" {
		t.Fatalf("process B lease=%q, want tbx_bee123", leaseB)
	}

	// Process B's healthy in-use testbox must survive process A's
	// failed-warmup cleanup: A never created it.
	if stopped := cp.stoppedIDs(); len(stopped) != 0 {
		t.Fatalf("process A's failed-warmup cleanup stopped testbox(es) it does not own: %v (process B's lease severed mid-run)", stopped)
	}
	if !cp.has("tbx_bee123") {
		t.Fatal("process B's testbox tbx_bee123 was removed from the control plane by process A's cleanup")
	}
	claim, claimErr := readLeaseClaim("tbx_bee123")
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if claim.LeaseID != "tbx_bee123" {
		t.Fatalf("process B's lease claim was removed by process A's cleanup: %#v", claim)
	}
}
