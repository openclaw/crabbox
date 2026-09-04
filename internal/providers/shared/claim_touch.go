package shared

import (
	"context"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ClaimTouchPolicy keeps provider identity and label preparation local. Prepare
// runs only after authorization, which may hydrate the recorded runtime scope.
// Neither callback may mutate a remote resource or publish the local claim.
type ClaimTouchPolicy struct {
	Provider  string
	Authorize func(context.Context, core.LeaseTarget, core.LeaseClaim) error
	Prepare   func(core.LeaseClaim) (map[string]string, time.Time)
}

// CommitClaimTouch publishes one prepared touch through the existing claim CAS.
// The returned claim is the committed snapshot; projection and runtime caches
// must be updated by the adapter only after this succeeds.
func CommitClaimTouch(ctx context.Context, req core.TouchRequest, policy ClaimTouchPolicy) (core.LeaseClaim, error) {
	expected, exists, set := core.ServerLeaseClaimSnapshot(req.Lease.Server)
	if !set || !exists {
		return core.LeaseClaim{}, core.Exit(4, "%s lease %s has no exact claim snapshot; refusing touch", policy.Provider, req.Lease.LeaseID)
	}
	if err := policy.Authorize(ctx, req.Lease, expected); err != nil {
		return core.LeaseClaim{}, err
	}
	if req.IdleTimeoutOverride != nil && *req.IdleTimeoutOverride <= 0 {
		return core.LeaseClaim{}, core.Exit(2, "%s lease %s idle timeout override must be positive", policy.Provider, req.Lease.LeaseID)
	}
	labels, now := policy.Prepare(expected)
	return core.UpdateLeaseClaimTouchIfUnchanged(ctx, req.Lease.LeaseID, expected, labels, now, req.IdleTimeoutOverride)
}
