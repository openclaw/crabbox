package parallels

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

var errCloneRejected = errors.New("clone rejected")

// fixedParallelsCapacityFixture gives the fixture's host a maxVMs limit, so the
// capacity gate is live.
func fixedParallelsCapacityFixture(t *testing.T, maxVMs int) (*leaseBackend, *parallelsFixedRunner, core.AcquireRequest) {
	t.Helper()
	backend, runner, req := fixedParallelsFixture(t)
	backend.Cfg.Parallels.Hosts = []core.ParallelsHostConfig{{Name: "capped", MaxVMs: maxVMs}}
	// Warm the per-lease key directory. Creating it is a check-then-create in
	// shared lease code, so concurrent first-time callers can collide there;
	// that is unrelated to capacity and would otherwise mask what these tests
	// are about.
	if _, _, err := core.EnsureTestboxKeyForConfig(backend.Cfg, "cbx_000000000000"); err != nil {
		t.Fatal(err)
	}
	return backend, runner, req
}

// R10 — a fixed-ID clone submission holds the host capacity reservation.
//
// Concurrent warmups with different lease IDs each pass the advisory selection
// before either clone is visible, and their per-lease claim locks do not
// serialize them, so without a reservation spanning the count and the clone
// both consume the same last maxVMs slot.
func TestParallelsFixedCloneHoldsHostCapacityReservation(t *testing.T) {
	backend, runner, req := fixedParallelsCapacityFixture(t, 1)
	repoRoot := req.Repo.Root

	var wg sync.WaitGroup
	results := make([]error, 2)
	leases := []string{"cbx_0123456789ab", "cbx_0123456789ac"}
	for i, leaseID := range leases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = backend.Acquire(context.Background(), core.AcquireRequest{
				RequestedLeaseID: leaseID,
				RequestedSlug:    "quiet-lobster",
				Repo:             core.Repo{Root: filepath.Join(repoRoot, leaseID)},
			})
		}()
	}
	wg.Wait()

	succeeded := 0
	for i, err := range results {
		if err == nil {
			succeeded++
			continue
		}
		if !strings.Contains(err.Error(), "maxVMs capacity") {
			t.Fatalf("lease %s failed for an unrelated reason: %v", leases[i], err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d of 2 concurrent fixed acquires succeeded against maxVMs=1, want 1", succeeded)
	}
	if clones, _ := runner.counts(); clones != 1 {
		t.Fatalf("clone calls=%d against maxVMs=1, want 1", clones)
	}
	runner.mu.Lock()
	crabbox := 0
	for _, vm := range runner.vms {
		if strings.HasPrefix(vm.Name, "crabbox-") {
			crabbox++
		}
	}
	runner.mu.Unlock()
	if crabbox != 1 {
		t.Fatalf("host holds %d crabbox VMs, want 1 under maxVMs=1", crabbox)
	}
}

// R10b — a prepared retry is subject to capacity too.
//
// An attempt that recorded its intent but never cloned must re-check capacity
// on its pinned host before submitting, or a lease prepared while there was
// room can overcommit the host once other leases have filled it.
func TestParallelsFixedPreparedRetryRespectsCapacity(t *testing.T) {
	backend, runner, req := fixedParallelsCapacityFixture(t, 1)

	// The attempt is recorded, but its clone never lands.
	runner.cloneErr, runner.cloneCommit = errCloneRejected, false
	if _, err := backend.Acquire(context.Background(), req); err == nil {
		t.Fatal("a rejected clone must not report a usable lease")
	}
	runner.cloneErr = nil
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "prepared" {
		t.Fatalf("intent is not prepared for retry: %#v err=%v", claim.FixedCreateIntent, err)
	}

	// Meanwhile another lease fills the host.
	runner.seedAt("crabbox-cbx-ffffffffffff-other")

	_, err = backend.Acquire(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "maxVMs capacity") {
		t.Fatalf("prepared retry at capacity err=%v, want a maxVMs capacity refusal", err)
	}
	if clones, _ := runner.counts(); clones != 0 {
		t.Fatalf("clone calls=%d, want 0: the retry overcommitted the host", clones)
	}
}

// R10c — replaying an existing VM still works at capacity.
//
// The reservation guards creation, not reconciliation. A lease whose VM already
// exists is not consuming a new slot, so a full host must not make it
// unreplayable or unstoppable.
func TestParallelsFixedReplayAndReleaseWorkAtCapacity(t *testing.T) {
	backend, runner, req := fixedParallelsCapacityFixture(t, 1)
	first, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runnerCapacityState(t, runner), "at capacity") {
		t.Fatal("fixture did not reach capacity")
	}

	replay, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatalf("replay at capacity: %v", err)
	}
	if replay.Server.CloudID != first.Server.CloudID {
		t.Fatalf("replay vm=%q, want %q", replay.Server.CloudID, first.Server.CloudID)
	}
	if clones, _ := runner.counts(); clones != 1 {
		t.Fatalf("clone calls=%d, want 1", clones)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: replay, Force: true}); err != nil {
		t.Fatalf("release at capacity: %v", err)
	}
}

func runnerCapacityState(t *testing.T, runner *parallelsFixedRunner) string {
	t.Helper()
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, vm := range runner.vms {
		if strings.HasPrefix(vm.Name, "crabbox-") {
			return "at capacity"
		}
	}
	return "below capacity"
}
