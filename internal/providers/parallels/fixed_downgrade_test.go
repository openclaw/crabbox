package parallels

import (
	"context"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

// releasedParallelsClientCanonicalProvider is the claim-provider canonicalization
// that shipped in v0.63.0: it knows the fixed markers that existed then and
// nothing else. An unknown marker stays unmapped, so a released client's
// exact-ownership check cannot recognize the claim as an ordinary Parallels
// lease and refuses to delete the VM or remove the claim.
func releasedParallelsClientCanonicalProvider(provider string) string {
	switch strings.TrimSpace(provider) {
	case "aws-fixed-v1":
		return "aws"
	case "machine0-fixed-v1":
		return "machine0"
	case "daytona-fixed-v1":
		return "daytona"
	case "local-container-fixed-v1":
		return "local-container"
	}
	return strings.TrimSpace(provider)
}

// releasedParallelsClientOwnsClaim mirrors v0.63.0 exactParallelsClaimOwned:
// it reads the exact claim file and compares its canonical provider, lease ID,
// CloudID and host label. Its release and cleanup paths delete the VM and call
// RemoveLeaseClaim unconditionally once this returns true.
func releasedParallelsClientOwnsClaim(t *testing.T, leaseID, vmID, host string) bool {
	t.Helper()
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	return exists && releasedParallelsClientCanonicalProvider(claim.Provider) == "parallels" &&
		claim.LeaseID == leaseID && claim.CloudID == vmID &&
		strings.TrimSpace(claim.Labels["host"]) == host
}

// R1 — a released Parallels client cannot erase a fixed claim.
//
// v0.63.0 has no fixed-intent branch: once its exact-ownership check passes it
// deletes the VM and calls RemoveLeaseClaim, erasing the terminal tombstone so
// the same fixed ID can create a second VM. The downgrade-safe claim marker is
// the only thing that fences it out, so acquisition must never leave a claim
// that a released client recognizes as its own.
func TestParallelsFixedClaimFencesReleasedClientRelease(t *testing.T) {
	backend, _, req := fixedParallelsFixture(t)
	lease, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != core.FixedParallelsClaimProvider {
		t.Fatalf("fixed claim provider=%q, want the downgrade-safe %q marker", claim.Provider, core.FixedParallelsClaimProvider)
	}
	host := strings.TrimSpace(claim.Labels["host"])
	if releasedParallelsClientOwnsClaim(t, req.RequestedLeaseID, lease.Server.CloudID, host) {
		t.Fatal("a released Parallels client still recognizes the fixed claim as an ordinary lease and would delete its VM and claim")
	}

	// The current client must still route the same claim, or the fence would
	// simply break stop and cleanup instead of fencing older clients.
	owned, err := exactParallelsClaimOwned(req.RequestedLeaseID, lease.Server.CloudID, host)
	if err != nil {
		t.Fatal(err)
	}
	if !owned {
		t.Fatal("the current client no longer recognizes its own fixed Parallels claim")
	}
	resolved, ok, exact, err := core.ResolveLeaseClaimForProviderWithExact(req.RequestedLeaseID, "parallels")
	if err != nil || !ok || !exact || resolved.Provider != core.FixedParallelsClaimProvider {
		t.Fatalf("current-client routing resolved=%#v ok=%t exact=%t err=%v", resolved, ok, exact, err)
	}

	// The terminal tombstone must carry the same fence.
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		t.Fatal(err)
	}
	tombstone, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists {
		t.Fatalf("tombstone exists=%t err=%v", exists, err)
	}
	if tombstone.Provider != core.FixedParallelsClaimProvider {
		t.Fatalf("tombstone provider=%q, want %q", tombstone.Provider, core.FixedParallelsClaimProvider)
	}
	if releasedParallelsClientOwnsClaim(t, req.RequestedLeaseID, lease.Server.CloudID, host) {
		t.Fatal("a released Parallels client would prune the terminal tombstone")
	}
}
