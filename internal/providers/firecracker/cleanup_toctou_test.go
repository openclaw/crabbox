package firecracker

import (
	"context"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

// TestCleanupTOCTOUDeletesConcurrentlyReclaimedClaim proves the
// check-then-destroy window in (*backend).Cleanup (backend.go ~line 426-440):
// Cleanup decides via shouldCleanupRecord, then kills the VM and calls the
// UNGUARDED core.RemoveLeaseClaim — with no if-unchanged guard between check
// and act. If another crabbox process reclaims the same lease inside that
// window (Resolve/Touch rewrites the claim through the real claim API), the
// freshly-rewritten claim is deleted out from under the live session.
//
// The interleaving is injected deterministically at a real point INSIDE the
// window: releaseStateRecord invokes the injectable b.cleanupNetwork hook
// after shouldCleanupRecord has returned true and before
// core.RemoveLeaseClaim runs. In production this window spans the
// SIGTERM/SIGKILL + wait of the firecracker process (seconds), so a
// concurrent `crabbox resolve --reclaim` fits in it easily. The claim flock
// in claim.go only serializes individual file mutations, not this
// check-then-destroy sequence; namespaceinstance closes exactly this window
// with core.RemoveLeaseClaimIfUnchanged (backend.go:455,473), which
// firecracker does not use.
//
// EXPECTED (correct, guarded) behavior: process B's rewritten claim survives
// Cleanup, because a guarded removal would observe the claim changed after
// the cleanup decision and refuse to delete it.
// ACTUAL (buggy) behavior: the claim is gone -> the final assertion fails.
func TestCleanupTOCTOUDeletesConcurrentlyReclaimedClaim(t *testing.T) {
	cfg := lifecycleConfig(t)
	test := newLifecycleTestBackend(t, cfg)

	// Process B originally acquired the lease (state record + claim exist).
	lease, err := test.backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: test.repoRoot}})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(lease.LeaseID, providerName); err != nil || !ok {
		t.Fatalf("precondition: acquired claim ok=%v err=%v", ok, err)
	}

	// The firecracker process has exited, so process A's Cleanup decides
	// shouldCleanupRecord=true (reason=process_exited) from its snapshot.
	test.processes.alive[4242] = false

	// Process B concurrently reclaims the same lease INSIDE the
	// check->destroy window, via the real production claim entry point
	// (the same call chain (*backend).claimLeaseTarget uses on Resolve).
	reclaimed := false
	prevCleanupNetwork := test.backend.cleanupNetwork
	test.backend.cleanupNetwork = func(ctx context.Context, record leaseStateRecord) error {
		if err := prevCleanupNetwork(ctx, record); err != nil {
			return err
		}
		if record.LeaseID != lease.LeaseID || reclaimed {
			return nil
		}
		server := Server{
			CloudID:  record.VMID,
			Provider: providerName,
			Name:     record.VMID,
			Labels: map[string]string{
				"lease": record.LeaseID,
				"slug":  record.Slug,
				"state": "ready",
			},
		}
		if err := core.ClaimLeaseTargetForRepoConfig(record.LeaseID, record.Slug, cfg, server, lease.SSH, test.repoRoot, cfg.IdleTimeout, true); err != nil {
			t.Fatalf("concurrent reclaim (process B) failed: %v", err)
		}
		reclaimed = true
		return nil
	}

	// Process A runs `crabbox cleanup`.
	if err := test.backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if !reclaimed {
		t.Fatal("harness: concurrent reclaim hook never ran inside the cleanup window")
	}

	// B's freshly-rewritten claim must survive A's cleanup: a guarded
	// removal (core.RemoveLeaseClaimIfUnchanged with the pre-decision
	// snapshot, as namespaceinstance does) refuses to delete a claim that
	// changed after the cleanup decision. The unguarded
	// core.RemoveLeaseClaim deletes it unconditionally -> this fails.
	if _, ok, err := core.ResolveLeaseClaimForProvider(lease.LeaseID, providerName); err != nil || !ok {
		t.Fatalf("TOCTOU: Cleanup deleted the claim process B rewrote inside the check->destroy window (claim present=%v err=%v); unguarded core.RemoveLeaseClaim needs the *IfUnchanged guard", ok, err)
	}
}
