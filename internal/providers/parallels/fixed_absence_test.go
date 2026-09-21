package parallels

import (
	"context"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

// R6 — a name-keyed miss is not an absence proof.
//
// An acquired VM renamed to another crabbox-<same-lease-id>-<slug> name still
// exists and still belongs to this lease, but the recorded-name lookup misses
// it. Finalizing on that miss would write a terminal tombstone and drop the
// claim while the VM keeps running. Only the bound UUID being absent from the
// complete inventory proves the resource is gone.
func TestParallelsFixedReleaseChecksBoundUUIDBeforeFinalizingAbsence(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	lease, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	name := fixedLeaseVMName(t, req.RequestedLeaseID)
	renamed := "crabbox-" + strings.ReplaceAll(req.RequestedLeaseID, "_", "-") + "-other-slug"
	runner.rename(name, renamed)

	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, Force: true})
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("release of a renamed VM err=%v, want lease_id_conflict", err)
	}
	if _, deletes := runner.counts(); deletes != 0 {
		t.Fatalf("delete calls=%d, want 0", deletes)
	}
	vm, ok := runner.find(renamed)
	if !ok {
		t.Fatalf("renamed VM %q did not survive the refused release", renamed)
	}
	if vm.ID != lease.Server.CloudID {
		t.Fatalf("renamed VM uuid=%q, want %q", vm.ID, lease.Server.CloudID)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.FixedCreateIntent == nil {
		t.Fatalf("custody was not retained: exists=%t err=%v", exists, err)
	}
	if claim.FixedCreateIntent.State == "released" {
		t.Fatal("release reported success for a VM that is still running")
	}
	if claim.CloudImmutableID != lease.Server.CloudID {
		t.Fatalf("claim lost its bound UUID: %q", claim.CloudImmutableID)
	}

	// Restoring the name makes the same release succeed, so the guard is about
	// evidence rather than a permanently stuck lease.
	runner.rename(renamed, name)
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		t.Fatalf("release after restoring the recorded name: %v", err)
	}
	if _, deletes := runner.counts(); deletes != 1 {
		t.Fatalf("delete calls=%d, want 1", deletes)
	}
}
