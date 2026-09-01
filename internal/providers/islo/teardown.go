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
	// sandbox behind. When the reads do burn everything up to that reserve, the
	// confirmation afterwards is what gives way: the teardown then reports itself
	// unproven and keeps the claim, so the delete is still retryable. The reserve
	// buys nothing for a read that fails outright on an id-bound claim - that
	// teardown declines to delete at all (see requireIsloVerifiedTarget) - but it
	// is what lets a slow-but-successful read still reach the delete, and it is
	// what keeps the name-only delete of a claim that records no id alive.
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
	// observed records that the resource this lease owns was positively
	// identified under the current credentials during this teardown. Without it
	// a later 404 is indistinguishable from "these credentials were never able
	// to see this resource", so it must not count as proof of deletion - and for
	// an id-bound claim it is also what licenses the name-only DELETE at all.
	observed bool
	// nameAbsentBefore records that the name already answered 404 before we
	// touched anything, which makes DELETE a no-op.
	nameAbsentBefore bool
	// deleteIssued records whether a DELETE actually went out, so no message
	// can describe an action that did not happen.
	deleteIssued bool
}

// confirmObserved records a positive identification of the resource the claim
// owns. For a claim that records an immutable id the live response must carry
// that same id: a response that omits it shows only that *something* currently
// holds the name, which is exactly what a reusable name cannot prove. A claim
// with no recorded id has no id to match, so any live response identifies it as
// well as anything ever could.
func (t *isloTeardownTarget) confirmObserved(live isloIdentity) {
	if t.resourceID != "" && live.ID != t.resourceID {
		return
	}
	t.observed = true
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
// Reads before the delete are not advisory: the delete endpoint is name-only and
// an Islo sandbox name is reusable once its sandbox is deleted, so for a claim
// that records an immutable id the reads are what establishes that the recorded
// name still is the resource this lease owns. When they cannot establish it the
// teardown declines to delete and keeps the claim (see requireIsloVerifiedTarget).
// A claim that records no id has no identity to cross and keeps its name-only
// delete.
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
		if err := requireIsloVerifiedTarget(claim, target); err != nil {
			return isloTeardownOutcome{name: target.name}, err
		}
		if err := client.DeleteSandbox(ctx, target.name); err != nil {
			return isloTeardownOutcome{}, isloError("delete sandbox", err)
		}
		target.deleteIssued = true
	}
	proof, err := b.confirmIsloTeardown(ctx, client, claim, target)
	return isloTeardownOutcome{name: target.name, proof: proof}, err
}

// requireIsloVerifiedTarget is the fail-closed gate in front of the DELETE.
//
// `DELETE /sandboxes/{name}` is name-only, and an Islo sandbox name is a
// namespace slot rather than a resource: once a sandbox is deleted the name is
// free for a new one. So for a claim that records an immutable id, deleting the
// recorded name without having identified it as that id would cross the lease's
// resource boundary - and a control-plane read outage, the one situation where
// both identity reads fail, is exactly when the delete would go out blind. This
// refuses instead, and the caller keeps the claim, so the same teardown can be
// retried against the same recorded id once reads answer again.
//
// A claim that records no immutable id predates the identity binding. It has no
// identity to cross: the name is the only handle it has ever had, and deleting
// by that name is precisely what the pre-binding teardown always did, so the
// name-only delete remains correct for it.
//
// The competing cost is real - a sandbox that is not deleted keeps billing - so
// the refusal names the sandbox, says the sandbox may still be running, and says
// which command to retry.
func requireIsloVerifiedTarget(claim core.LeaseClaim, target isloTeardownTarget) error {
	if target.observed || isloClaimIdentity(claim).ID == "" {
		return nil
	}
	return exit(5, "islo refused to delete sandbox %q for lease %q (%s): the sandbox could not be read under these credentials during this teardown, so the recorded name could not be shown to still be the resource this lease owns, and an islo sandbox name is reusable once its sandbox is deleted; deleting it blind could destroy a different sandbox. The sandbox may still be running and billable: the claim is retained, so run `%s` to retry the delete once the islo API answers reads again", target.name, claim.LeaseID, isloIdentityString(target.identity()), isloCleanupCommand(claim.LeaseID))
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
		target.confirmObserved(live)
	case err != nil && !isloNotFound(err):
		// Not a 404: we learned nothing about this resource, so the by-name
		// lookup is the only remaining way to identify it.
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
		target.confirmObserved(live)
		if !target.observed {
			// The name answered, but the response carries no id, so it cannot
			// show that this name is still the resource the claim owns.
			b.warnf("warning: islo read sandbox %q by name but the response carries no resource id, so it cannot be matched against resource %s, which this lease owns\n", target.name, target.resourceID)
		}
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
