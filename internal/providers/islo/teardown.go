package islo

import (
	"context"
	"fmt"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// isloTeardownProof names the evidence that the exact resource a lease owned is
// gone. Only these proofs are accepted; in particular `GET /sandboxes` (list) is
// never consulted, because the live API keeps returning a deleted sandbox in the
// listing for seconds after `GET /sandboxes/{name}` already answers 404. A list
// is eventually consistent and cannot prove absence.
type isloTeardownProof string

const (
	// isloProofTombstone is the strongest proof: `GET /sandboxes/-/by-id/{id}`
	// answers 200 for a deleted sandbox with status "deleted" and deleted_at
	// set, which pins the exact resource generation the lease owned.
	isloProofTombstone isloTeardownProof = "tombstone"
	// isloProofNameAbsent is the fallback used when the tombstone is not
	// available: a 404 on the exact name, which the API answers immediately
	// after a delete. It is only accepted once the resource has been observed
	// under these very credentials during this same teardown.
	isloProofNameAbsent isloTeardownProof = "name-404"
	// isloProofNameAbsentUnbound is the weakest proof, and it exists so that a
	// claim written before the identity binding cannot become permanently
	// unreleasable. Such a claim records no resource id, so no better evidence
	// is obtainable: a 404 on its name is the same evidence the pre-binding
	// teardown implicitly relied on. It is reported distinctly and warned about
	// rather than silently equated with the stronger proofs.
	isloProofNameAbsentUnbound isloTeardownProof = "name-404-unbound"
)

// isloTeardownBudgets bounds a teardown for a caller that cannot wait
// indefinitely. A zero overall budget inherits the caller's context unchanged,
// which is what the interactive `crabbox stop` path wants.
type isloTeardownBudgets struct {
	// overall bounds the whole teardown: every read plus the DELETE. The run
	// cleanup defer runs while the user waits, so a hung control plane must cost
	// them one budget, not one per API call.
	overall time.Duration
	// deleteReserve is the slice of overall that the pre-flight identity reads
	// may not consume, because a DELETE that never goes out leaves a billed
	// sandbox behind - the worst outcome available here. When the reads do burn
	// everything up to that reserve, the confirmation afterwards is what gives
	// way: the teardown then reports itself unproven and keeps the claim, so the
	// delete is still retryable.
	deleteReserve time.Duration
}

// bound applies the overall budget. The DELETE runs on this context directly, so
// it is bounded by the whole-teardown deadline and nothing shorter.
func (budgets isloTeardownBudgets) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	if budgets.overall <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, budgets.overall)
}

// preDeleteReadContext shortens an identity read so the DELETE's reserved slice
// of the overall budget survives it.
func (budgets isloTeardownBudgets) preDeleteReadContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok || budgets.deleteReserve <= 0 {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, deadline.Add(-budgets.deleteReserve))
}

// isloTeardownTarget accumulates what a teardown learned about the resource it
// is acting on. `name` is what DELETE is aimed at (the endpoint is name-only),
// `resourceID` is what the proof is anchored on.
type isloTeardownTarget struct {
	name       string
	resourceID string
	// observed records that the resource answered under the current credentials
	// during this teardown. Without it a later 404 is indistinguishable from
	// "these credentials were never able to see this resource", so it must not
	// count as proof of deletion.
	observed bool
	// nameAbsentBefore records that the name already answered 404 before we
	// touched anything, which makes DELETE a no-op.
	nameAbsentBefore bool
	// deleteIssued records whether a DELETE actually went out, so no message
	// can describe an action that did not happen.
	deleteIssued bool
}

func (t isloTeardownTarget) identity() isloIdentity {
	return isloIdentity{ID: t.resourceID, Name: t.name}
}

func (t isloTeardownTarget) deleteNarrative() string {
	if t.deleteIssued {
		return "a delete was issued"
	}
	return "no delete was issued"
}

// isloTeardownOutcome reports what the teardown actually acted on. DELETE is
// aimed at the name the claimed resource id resolves to, which is not
// necessarily the name the caller derived, so the caller must report this name
// rather than the one it asked about.
type isloTeardownOutcome struct {
	name  string
	proof isloTeardownProof
}

// teardownIsloSandbox deletes the exact resource a claim owns and reports what
// it acted on, with the proof that the resource is gone. The caller must drop the local claim only on a nil
// error: every uncertain outcome (network failure, ambiguous status, a resource
// these credentials cannot address) returns an error and leaves the claim in
// place, because the claim is the only handle a later `crabbox stop` has for
// retrying the teardown. Dropping it on an unproven delete is how a paid
// sandbox becomes unreachable garbage.
//
// Reads before the delete inform the confirmation and can refuse on a positive
// identity mismatch. A read that merely FAILS must never suppress the delete:
// the sandbox is billed for as long as it exists, so a transient control-plane
// error falls through to the delete attempt.
func (b *isloBackend) teardownIsloSandbox(ctx context.Context, client isloAPI, claim core.LeaseClaim, name string, budgets isloTeardownBudgets) (isloTeardownOutcome, error) {
	if err := requireIsloClaimScope(claim, b.claimScope()); err != nil {
		return isloTeardownOutcome{}, err
	}
	ctx, cancel := budgets.bound(ctx)
	defer cancel()
	bound := isloClaimIdentity(claim)
	target := isloTeardownTarget{
		name:       blank(bound.Name, name),
		resourceID: bound.ID,
	}
	if target.resourceID != "" {
		done, proof, err := b.locateIsloTargetByID(ctx, client, claim, &target, budgets)
		if err != nil || done {
			return isloTeardownOutcome{name: target.name, proof: proof}, err
		}
	}
	if !target.observed {
		if err := b.locateIsloTargetByName(ctx, client, claim, &target, budgets); err != nil {
			return isloTeardownOutcome{}, err
		}
	}
	if !target.nameAbsentBefore {
		if err := client.DeleteSandbox(ctx, target.name); err != nil {
			return isloTeardownOutcome{}, isloError("delete sandbox", err)
		}
		target.deleteIssued = true
	}
	proof, err := b.confirmIsloTeardown(ctx, client, claim, target)
	return isloTeardownOutcome{name: target.name, proof: proof}, err
}

// locateIsloTargetByID resolves the claimed resource id. It reports done=true
// when the resource is already tombstoned, in which case there is nothing left
// to delete. A live hit supplies the name DELETE must use: the delete endpoint
// takes a name, and the authoritative name is the one the id resolves to, not
// the one the caller guessed.
func (b *isloBackend) locateIsloTargetByID(ctx context.Context, client isloAPI, claim core.LeaseClaim, target *isloTeardownTarget, budgets isloTeardownBudgets) (bool, isloTeardownProof, error) {
	readCtx, cancel := budgets.preDeleteReadContext(ctx)
	sandbox, err := client.GetSandboxByID(readCtx, target.resourceID)
	cancel()
	switch {
	case err == nil && sandbox != nil:
		live := isloIdentityFromSandbox(sandbox)
		if live.Name != "" {
			target.name = live.Name
		}
		if isloSandboxDeleted(sandbox) {
			return true, isloProofTombstone, nil
		}
		advisory, err := requireIsloIdentityMatch(claim, live)
		if err != nil {
			return true, "", err
		}
		b.warnIsloAdvisory(advisory)
		target.observed = true
	case err != nil && !isloNotFound(err):
		// Not a 404: we learned nothing, and that must not stop the delete.
		b.warnf("warning: islo could not read sandbox %s by id before delete: %v\n", target.resourceID, err)
	}
	// A 404 from the by-id lookup means the resource is not visible to these
	// credentials, not that it is gone. Fall through to the by-name path.
	return false, "", nil
}

func (b *isloBackend) locateIsloTargetByName(ctx context.Context, client isloAPI, claim core.LeaseClaim, target *isloTeardownTarget, budgets isloTeardownBudgets) error {
	readCtx, cancel := budgets.preDeleteReadContext(ctx)
	sandbox, err := client.GetSandbox(readCtx, target.name)
	cancel()
	switch {
	case isloSandboxAbsentByName(sandbox, err):
		target.nameAbsentBefore = true
	case err != nil:
		b.warnf("warning: islo could not read sandbox %q by name before delete: %v\n", target.name, err)
	default:
		live := isloIdentityFromSandbox(sandbox)
		advisory, err := requireIsloIdentityMatch(claim, live)
		if err != nil {
			return err
		}
		b.warnIsloAdvisory(advisory)
		target.observed = true
		if target.resourceID == "" {
			// Claims written before the identity binding carry no id. The live
			// response hands us one, so the tombstone check works for them too.
			target.resourceID = live.ID
		}
	}
	return nil
}

func (b *isloBackend) confirmIsloTeardown(ctx context.Context, client isloAPI, claim core.LeaseClaim, target isloTeardownTarget) (isloTeardownProof, error) {
	if target.resourceID != "" {
		tombstone, err := client.GetSandboxByID(ctx, target.resourceID)
		switch {
		case err == nil && tombstone != nil:
			if isloSandboxDeleted(tombstone) {
				return isloProofTombstone, nil
			}
			return "", isloTeardownUnproven(claim, target, fmt.Sprintf("resource %s still reports status %q", target.resourceID, tombstone.GetStatus()))
		case err != nil && !isloNotFound(err):
			return "", isloError("confirm sandbox deletion by id", err)
		}
		// A 404 from the by-id lookup means the authoritative tombstone is not
		// available to us, not that the resource is gone. Fall through.
	}
	absent := target.nameAbsentBefore
	if !absent {
		sandbox, err := client.GetSandbox(ctx, target.name)
		switch {
		case isloSandboxAbsentByName(sandbox, err):
			absent = true
		case err != nil:
			return "", isloError("confirm sandbox deletion by name", err)
		default:
			return "", isloTeardownUnproven(claim, target, fmt.Sprintf("sandbox %q still resolves by name with status %q", target.name, sandbox.GetStatus()))
		}
	}
	switch {
	case target.observed:
		return isloProofNameAbsent, nil
	case isloClaimIdentity(claim).ID == "":
		// The claim predates the identity binding, so no stronger evidence can
		// exist for it. Accepting the name-404 here is what keeps such a claim
		// releasable; refusing it would leave the lease unusable forever, with
		// no prune command to fall back on.
		b.warnf("warning: islo released lease %s on a name-only 404: the claim records no resource id, so the delete could not be confirmed against a specific resource. Re-create the lease to get an id-anchored claim.\n", claim.LeaseID)
		return isloProofNameAbsentUnbound, nil
	default:
		return "", exit(5, "islo cannot prove lease %q (%s) is deleted: no by-id tombstone, and the sandbox was never observed under these credentials during this teardown, which is indistinguishable from a resource they cannot address (%s); retaining the claim so `%s` can retry with the owning credentials", claim.LeaseID, isloIdentityString(target.identity()), target.deleteNarrative(), isloCleanupCommand(claim.LeaseID))
	}
}

// isloTeardownUnproven reports an uncertain teardown. The message states
// whether a delete was actually issued, so it can never describe an action that
// did not happen.
func isloTeardownUnproven(claim core.LeaseClaim, target isloTeardownTarget, detail string) error {
	return exit(5, "islo teardown of lease %q (%s) is unproven: %s (%s); retaining the claim so `%s` can retry", claim.LeaseID, isloIdentityString(target.identity()), detail, target.deleteNarrative(), isloCleanupCommand(claim.LeaseID))
}

func (b *isloBackend) warnIsloAdvisory(advisory string) {
	if advisory == "" {
		return
	}
	b.warnf("warning: %s\n", advisory)
}
