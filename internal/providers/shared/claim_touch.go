package shared

import (
	"context"
	"strconv"
	"strings"
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

// ClaimActivityHoldState identifies logical holds that native runtime status
// cannot clear. Adapters must retain them in observations and reject activity.
func ClaimActivityHoldState(claim core.LeaseClaim) string {
	state := strings.ToLower(strings.TrimSpace(claim.Labels["state"]))
	switch state {
	case "cleanup", "deleting", "expired", "released":
		return state
	default:
		return ""
	}
}

// AuthorizeClaimActivity checks lifecycle holds and checkpoint exclusion;
// resource ownership and exact-snapshot checks remain the adapter's responsibility.
func AuthorizeClaimActivity(claim core.LeaseClaim) error {
	if hold := ClaimActivityHoldState(claim); hold != "" {
		return core.Exit(4, "%s lease=%s activity is held in state %s", claim.Provider, claim.LeaseID, hold)
	}
	return core.AuthorizeCheckpointRelease(claim, "")
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

// ClaimLifecycleLabels restores persisted lifecycle policy without renewing it.
// It retains all internal provider metadata; it is not a public-label filter.
func ClaimLifecycleLabels(claim core.LeaseClaim) map[string]string {
	labels := CloneLabels(claim.Labels)
	if claim.IdleTimeoutSeconds > 0 {
		labels["idle_timeout"] = strconv.Itoa(claim.IdleTimeoutSeconds)
		labels["idle_timeout_secs"] = labels["idle_timeout"]
	}
	for key, value := range map[string]string{"created_at": claim.ClaimedAt, "last_touched_at": claim.LastUsedAt} {
		if labels[key] == "" {
			if stamp, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
				labels[key] = core.LeaseLabelTime(stamp)
			}
		}
	}
	return labels
}
