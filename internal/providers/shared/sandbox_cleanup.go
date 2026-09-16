package shared

import (
	"context"
	"fmt"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// SandboxClaimCleanup describes the existing claim-backed sandbox cleanup
// protocol. Adapters own scope admission, locking, resource identity, expiry,
// and provider mutations. Special claims remain entirely adapter-owned.
type SandboxClaimCleanup[T any] struct {
	Provider          string
	Runtime           core.Runtime
	Now               time.Time
	MatchesScope      func(core.LeaseClaim) bool
	Lock              func(context.Context, string) (func(), error)
	SandboxID         func(core.LeaseClaim) string
	Get               func(context.Context, string) (T, error)
	Delete            func(context.Context, string) error
	IsNotFound        func(error) bool
	ForgetMissing     bool
	ForgetMissingHint string
	Due               func(core.LeaseClaim, time.Time) (bool, string)
	Validate          func(core.LeaseClaim, T) error
	// Special runs after locked reread/admission, before normal resource lookup.
	Special func(context.Context, core.LeaseClaim) (handled, removed, claimRemoved bool, err error)
}

type sandboxCleanupOutcome struct{ checked, removed, claimRemoved bool }

// CleanupSandboxClaims keeps each operation lock through claim removal and
// reporting. The first failure ends the scan without a success summary.
func CleanupSandboxClaims[T any](ctx context.Context, req core.CleanupRequest, claims []core.LeaseClaim, cleanup SandboxClaimCleanup[T]) error {
	checked, removed, claimsRemoved := 0, 0, 0
	for _, listed := range claims {
		if listed.Provider != cleanup.Provider || !cleanup.MatchesScope(listed) {
			continue
		}
		outcome, err := cleanup.one(ctx, req.DryRun, listed)
		if err != nil {
			return err
		}
		if outcome.checked {
			checked++
		}
		if outcome.removed {
			removed++
		}
		if outcome.claimRemoved {
			claimsRemoved++
		}
	}
	if !req.DryRun {
		fmt.Fprintf(cleanup.Runtime.Stdout, "%s cleanup removed=%d claims_removed=%d checked=%d\n", cleanup.Provider, removed, claimsRemoved, checked)
	}
	return nil
}

func (c SandboxClaimCleanup[T]) one(ctx context.Context, dryRun bool, listed core.LeaseClaim) (outcome sandboxCleanupOutcome, err error) {
	unlock, err := c.Lock(ctx, listed.LeaseID)
	if err != nil {
		return outcome, err
	}
	defer unlock()
	claim, err := core.ReadLeaseClaim(listed.LeaseID)
	if err != nil {
		return outcome, err
	}
	if claim.LeaseID == "" || claim.Provider != c.Provider || !c.MatchesScope(claim) {
		return outcome, nil
	}
	outcome.checked = true
	if c.Special != nil {
		handled, removed, claimRemoved, err := c.Special(ctx, claim)
		if handled || err != nil {
			outcome.removed, outcome.claimRemoved = removed, claimRemoved
			return outcome, err
		}
	}
	sandboxID := c.SandboxID(claim)
	sandbox, getErr := c.Get(ctx, sandboxID)
	if getErr != nil {
		if !c.IsNotFound(getErr) {
			return outcome, getErr
		}
		if !c.ForgetMissing {
			fmt.Fprintf(c.Runtime.Stderr, "skip sandbox=%s lease=%s reason=missing-or-inaccessible; set %s to remove the claim\n", sandboxID, claim.LeaseID, c.ForgetMissingHint)
			return outcome, nil
		}
		if dryRun {
			fmt.Fprintf(c.Runtime.Stdout, "would remove claim lease=%s slug=%s reason=missing sandbox\n", claim.LeaseID, core.Blank(claim.Slug, "-"))
			return outcome, nil
		}
		if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			return outcome, err
		}
		fmt.Fprintf(c.Runtime.Stdout, "remove claim lease=%s slug=%s reason=missing sandbox\n", claim.LeaseID, core.Blank(claim.Slug, "-"))
		outcome.claimRemoved = true
		return outcome, nil
	}
	due, reason := c.Due(claim, c.Now)
	if !due {
		fmt.Fprintf(c.Runtime.Stderr, "skip sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
		return outcome, nil
	}
	if c.Validate != nil {
		if err := c.Validate(claim, sandbox); err != nil {
			return outcome, err
		}
	}
	if dryRun {
		fmt.Fprintf(c.Runtime.Stdout, "would delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
		return outcome, nil
	}
	if err := c.Delete(ctx, sandboxID); err != nil && !c.IsNotFound(err) {
		return outcome, err
	}
	if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
		return outcome, err
	}
	fmt.Fprintf(c.Runtime.Stdout, "delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
	outcome.removed = true
	return outcome, nil
}
