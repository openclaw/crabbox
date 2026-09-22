package cli

import (
	"context"
	"fmt"
	"reflect"
	"time"
)

// AbsenceVerifier only observes the exact bound resource. It must attest the
// claim's scope/account, an exact structured not-found, and complete unfiltered
// inventory absence wherever listing is supported. Errors never prove absence.
type AbsenceVerifier interface {
	VerifyResourceAbsent(context.Context, LeaseClaim) (AbsenceEvidence, error)
}

// AbsenceEvidence binds proof to the entire claim, including its revision and
// immutable resource identity. Zero evidence means the resource is observable.
type AbsenceEvidence struct {
	Claim             LeaseClaim
	ExactNotFound     bool
	InventoryComplete bool
}

type AbsenceVerifierFunc func(context.Context, LeaseClaim) (AbsenceEvidence, error)

func (f AbsenceVerifierFunc) VerifyResourceAbsent(ctx context.Context, claim LeaseClaim) (AbsenceEvidence, error) {
	return f(ctx, claim)
}

// OrdinaryStopAbsenceRecovery preserves an adapter's existing ordinary-stop
// contract. Other capable adapters require the targeted --force recovery path.
type OrdinaryStopAbsenceRecovery interface {
	ReconcileAbsenceOnOrdinaryStop() bool
}

// VerifyClaimResourceAbsence does not change local state. Callers that finalize
// a claim must use ForgetAbsentLeaseClaim to recheck under its exclusive fence.
func VerifyClaimResourceAbsence(ctx context.Context, verifier AbsenceVerifier, claim LeaseClaim) (bool, error) {
	if !IsCanonicalLeaseID(claim.LeaseID) || claim.Revision == "" || claim.Provider == "" || claim.CloudID == "" {
		return false, Exit(4, "absence recovery requires an exact local claim")
	}
	if claim.FixedCreateIntent != nil || claim.CheckpointCapture != nil || claim.CoordinatorRegistrationURL != "" || claim.RuntimeAdapterRegistrationID != "" || claim.RuntimeAdapterPendingRegistrationID != "" {
		return false, Exit(4, "claim has a fixed, checkpoint, coordinator, or adapter owner; absence recovery cannot forget it")
	}
	if err := AuthorizeCheckpointRelease(claim, ""); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	evidence, err := verifier.VerifyResourceAbsent(ctx, cloneLeaseClaim(claim))
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if reflect.DeepEqual(evidence, AbsenceEvidence{}) {
		return false, nil
	}
	if claim.ProviderScope == "" || !evidence.ExactNotFound || !evidence.InventoryComplete || !reflect.DeepEqual(evidence.Claim, claim) {
		return false, Exit(4, "resource absence evidence is incomplete or does not match the exact claim; retaining claim")
	}
	if err := AuthorizeCheckpointRelease(claim, ""); err != nil {
		return false, err
	}
	return true, nil
}

// ForgetAbsentLeaseClaim owns only local claim removal, never release, keys,
// registrations, or terminal receipts. The same fence covers proof and removal.
func ForgetAbsentLeaseClaim(ctx context.Context, verifier AbsenceVerifier, claim LeaseClaim) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	forgotten := false
	err := finalizeLeaseClaimIfUnchangedAfterContext(ctx, claim.LeaseID, claim, true, func() (bool, error) {
		var err error
		forgotten, err = VerifyClaimResourceAbsence(ctx, verifier, claim)
		return forgotten, err
	}, syncControllerDirectory)
	return forgotten && err == nil, err
}

func (a App) recoverAbsentStopClaim(ctx context.Context, backend Backend, id string, force bool) (handled, verified bool, err error) {
	verifier, ok := backend.(AbsenceVerifier)
	policy, ordinary := backend.(OrdinaryStopAbsenceRecovery)
	if !ok || backendCoordinator(backend) != nil || !force && (!ordinary || !policy.ReconcileAbsenceOnOrdinaryStop()) {
		return false, false, nil
	}
	if force && !IsCanonicalLeaseID(id) {
		if _, adopts := backend.(StopReclaimBackend); adopts {
			return false, false, nil
		}
		return false, false, Exit(2, "absence recovery requires an exact canonical claim --id")
	}
	claim, exists, err := ResolveLeaseClaimForProvider(id, backend.Spec().Name)
	if err == nil && !exists && !force && !IsCanonicalLeaseID(id) {
		claim, exists, err = ResolveLeaseClaimForProviderCloudID(id, backend.Spec().Name)
	}
	if err != nil || !exists {
		if err == nil && force {
			if _, adopts := backend.(StopReclaimBackend); !adopts {
				err = Exit(4, "absence recovery requires an existing exact local claim for %s", id)
			}
		}
		return false, false, err
	}
	if claim.Provider != backend.Spec().Name || force && claim.LeaseID != id {
		return false, false, Exit(4, "absence recovery claim identity does not match the selected provider and ID")
	}
	// Fixed leases retain their own deletion fence and terminal tombstone.
	if !force && claim.FixedCreateIntent != nil {
		return false, false, nil
	}
	forgotten, err := ForgetAbsentLeaseClaim(ctx, verifier, claim)
	if forgotten {
		fmt.Fprintf(a.Stderr, "lease=%s forgotten locally (resource absent)\n", claim.LeaseID)
	}
	return forgotten, err == nil, err
}
