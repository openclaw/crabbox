package parallels

import (
	"context"
	"errors"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

// R3 — a replacement VM at the recorded name never inherits an unbound
// attempt's authority.
//
// A lost clone reply leaves the attempt with no observed VM UUID. If the
// original VM is then replaced under the same host-unique name, the name alone
// must not authorize adoption: the replacement would receive the per-lease SSH
// key and become deletable through the same validator. Rejection has to happen
// before any guest mutation and before any delete.
func TestParallelsFixedUnboundAttemptRejectsReplacementVM(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	runner.cloneErr, runner.cloneCommit = errors.New("reply lost after commit"), true
	if _, err := backend.Acquire(context.Background(), req); err == nil {
		t.Fatal("a lost clone reply must not report a usable lease")
	}
	runner.cloneErr, runner.cloneCommit = nil, false

	name := fixedLeaseVMName(t, req.RequestedLeaseID)
	original, ok := runner.find(name)
	if !ok {
		t.Fatalf("fixture did not commit VM %q", name)
	}
	// The attempt's clone is removed and a VM created by some other operation
	// takes its name. It lives in the host's ordinary VM directory rather than
	// in this attempt's creation directory, which is what distinguishes it.
	runner.remove(original.ID)
	replacement := runner.seedAt(name)
	if replacement.ID == original.ID {
		t.Fatal("fixture did not replace the VM incarnation at the recorded name")
	}

	_, err := backend.Acquire(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("replay onto a replacement VM err=%v, want lease_id_conflict", err)
	}

	runner.mu.Lock()
	execs, starts := len(runner.execCalls), len(runner.startCalls)
	runner.mu.Unlock()
	if execs != 0 {
		t.Fatalf("guest exec calls=%d, want 0: the replacement VM received guest mutation", execs)
	}
	if starts != 0 {
		t.Fatalf("start calls=%d, want 0: the replacement VM was powered on", starts)
	}
	if _, deletes := runner.counts(); deletes != 0 {
		t.Fatalf("delete calls=%d, want 0: the replacement VM was deleted", deletes)
	}
	if _, ok := runner.find(name); !ok {
		t.Fatalf("VM %q was removed by a refused replay", name)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.FixedCreateIntent == nil {
		t.Fatalf("custody was not retained: exists=%t err=%v", exists, err)
	}
	if claim.CloudID == replacement.ID || claim.CloudImmutableID == replacement.ID {
		t.Fatalf("a refused replay bound the replacement %q", replacement.ID)
	}
}

// R3b — release will not delete an unattested VM either.
//
// releaseFixed uses the same validator, so without the attempt's recorded UUID
// it must retain custody rather than delete whatever occupies the name.
func TestParallelsFixedReleaseRefusesUnattestedVM(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	runner.cloneErr, runner.cloneCommit = errors.New("reply lost after commit"), true
	if _, err := backend.Acquire(context.Background(), req); err == nil {
		t.Fatal("a lost clone reply must not report a usable lease")
	}
	runner.cloneErr, runner.cloneCommit = nil, false
	name := fixedLeaseVMName(t, req.RequestedLeaseID)

	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{LeaseID: req.RequestedLeaseID},
		Force: true,
	})
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("release of an unattested VM err=%v, want lease_id_conflict", err)
	}
	if _, deletes := runner.counts(); deletes != 0 {
		t.Fatalf("delete calls=%d, want 0", deletes)
	}
	if _, ok := runner.find(name); !ok {
		t.Fatalf("VM %q was deleted without incarnation attestation", name)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.FixedCreateIntent == nil {
		t.Fatalf("custody was not retained: exists=%t err=%v", exists, err)
	}
	if claim.FixedCreateIntent.State == "released" {
		t.Fatal("a refused release wrote a terminal tombstone")
	}
}

// R3c — the creation evidence is durable before the clone, so a failed read
// afterwards still leaves the attempt attributable.
//
// prlctl clone reports no UUID, so the UUID can only be learned by reading the
// host back. What must be durable beforehand is the evidence that says which VM
// this attempt created: its own --dst directory, recorded before the clone runs.
func TestParallelsFixedCreationEvidenceIsDurableBeforeClone(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	var atClone string
	runner.beforeClone = func() {
		claim, _, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
		if err != nil {
			t.Error(err)
			return
		}
		if claim.FixedCreateIntent != nil {
			atClone = claim.FixedCreateIntent.Attempt["dst"]
		}
	}
	// The clone succeeds; the complete inventory read that reconciles it
	// afterwards does not. afterClone is invoked under the runner lock.
	runner.afterClone = func() {
		runner.listAllErr = errors.New("host unreachable after clone")
	}
	if _, err := backend.Acquire(context.Background(), req); err == nil {
		t.Fatal("a failed post-clone reconcile must not report a usable lease")
	}
	if strings.TrimSpace(atClone) == "" {
		t.Fatal("no creation directory was durable before prlctl clone ran")
	}

	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if got := claim.FixedCreateIntent.Attempt["dst"]; got != atClone {
		t.Fatalf("creation directory changed after the clone: %q then %q", atClone, got)
	}
	name := core.ParallelsLeaseVMName(req.RequestedLeaseID, claim.FixedCreateIntent.Slug)
	created, ok := runner.find(name)
	if !ok {
		t.Fatalf("fixture did not commit VM %q", name)
	}
	if !strings.HasPrefix(created.Home, strings.TrimRight(atClone, "/")+"/") {
		t.Fatalf("clone home=%q, want it inside the attempt directory %q", created.Home, atClone)
	}

	// The evidence survives, so replay recovers its own VM rather than cloning
	// a second one or retaining uncertain custody.
	runner.afterClone = nil
	runner.mu.Lock()
	runner.listAllErr = nil
	runner.mu.Unlock()
	lease, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatalf("replay of an attested attempt: %v", err)
	}
	if lease.Server.CloudID != created.ID {
		t.Fatalf("adopted vm=%q, want %q", lease.Server.CloudID, created.ID)
	}
	if clones, _ := runner.counts(); clones != 1 {
		t.Fatalf("clone calls=%d, want 1", clones)
	}
}

// R9 — replay does not write to the guest again.
//
// An intent reaches `acquired` only after the per-lease key is installed and
// the guest is ready. Repeating that on replay is redundant guest mutation, and
// it makes an otherwise valid replay depend on the guest-tools channel being
// available. Readiness is still re-proved over SSH.
func TestParallelsFixedReplayDoesNotRewriteTheGuest(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	if _, err := backend.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	afterAcquire := len(runner.execCalls)
	runner.mu.Unlock()
	if afterAcquire == 0 {
		t.Fatal("acquisition did not prepare the guest at all")
	}

	if _, err := backend.Acquire(context.Background(), req); err != nil {
		t.Fatalf("replay: %v", err)
	}
	runner.mu.Lock()
	afterReplay := len(runner.execCalls)
	runner.mu.Unlock()
	if afterReplay != afterAcquire {
		t.Fatalf("replay issued %d additional guest command(s), want 0", afterReplay-afterAcquire)
	}
}

// R9b — a lease that never finished preparing is still prepared on replay.
func TestParallelsFixedReplayPreparesUnfinishedGuest(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	rewriteFixedClaimAfter := func() {
		rewriteFixedClaim(t, req.RequestedLeaseID, func(claim *core.LeaseClaim) {
			claim.FixedCreateIntent.State = "prepared"
		})
	}
	if _, err := backend.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rewriteFixedClaimAfter()
	runner.mu.Lock()
	before := len(runner.execCalls)
	runner.mu.Unlock()

	if _, err := backend.Acquire(context.Background(), req); err != nil {
		t.Fatalf("replay of an unfinished lease: %v", err)
	}
	runner.mu.Lock()
	after := len(runner.execCalls)
	runner.mu.Unlock()
	if after <= before {
		t.Fatal("replay of a lease that never reached acquired skipped guest preparation")
	}
}
