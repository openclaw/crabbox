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
	// A different VM incarnation now occupies the recorded name.
	runner.replaceUUID(name, "{replacement-vm-uuid}")
	if replacement, ok := runner.find(name); !ok || replacement.ID == original.ID {
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
	if claim.CloudID != "" || claim.CloudImmutableID != "" {
		t.Fatalf("a refused replay bound resource identity: %q/%q", claim.CloudID, claim.CloudImmutableID)
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

// R3c — the UUID a successful clone returned is durable before any later read.
//
// Reconciliation after the clone can fail; if the attempt were still unbound at
// that point, a later replay would be unable to attest its own VM and would
// fall into uncertain custody for a lease that in fact succeeded.
func TestParallelsFixedBindsCloneUUIDBeforeReconciliation(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	// The clone succeeds and reports its UUID; the complete inventory read that
	// reconciles it afterwards does not. afterClone is invoked under the runner
	// lock, so it writes directly.
	runner.afterClone = func() {
		runner.listAllErr = errors.New("host unreachable after clone")
	}
	if _, err := backend.Acquire(context.Background(), req); err == nil {
		t.Fatal("a failed post-clone reconcile must not report a usable lease")
	}

	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	name := core.ParallelsLeaseVMName(req.RequestedLeaseID, claim.FixedCreateIntent.Slug)
	created, ok := runner.find(name)
	if !ok {
		t.Fatalf("fixture did not commit VM %q", name)
	}
	if got := claim.FixedCreateIntent.Attempt["vm_uuid"]; got != created.ID {
		t.Fatalf("attempt vm_uuid=%q, want the clone's %q persisted before reconciliation", got, created.ID)
	}
	if claim.CloudImmutableID != created.ID {
		t.Fatalf("claim immutable id=%q, want %q", claim.CloudImmutableID, created.ID)
	}

	// With the incarnation bound, replay adopts its own VM rather than
	// retaining uncertain custody.
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
