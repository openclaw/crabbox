package cli

import (
	"fmt"
	"strings"
	"time"
)

type FixedLeaseKind struct {
	ClaimProvider string
	IntentVersion int
	Label         string
}

func (k FixedLeaseKind) IsFixedClaim(claim LeaseClaim) bool {
	return claim.Provider == k.ClaimProvider && claim.FixedCreateIntent != nil
}

func (k FixedLeaseKind) TerminalClaim(claim LeaseClaim, now time.Time) LeaseClaim {
	intent := *claim.FixedCreateIntent
	intent.State = "released"
	intent.Attempt = nil
	intent.FailedAttempts = nil
	return LeaseClaim{
		LeaseID:           claim.LeaseID,
		Slug:              intent.Slug,
		Provider:          k.ClaimProvider,
		ProviderScope:     intent.ProviderScope,
		ClaimedAt:         claim.ClaimedAt,
		LastUsedAt:        now.Format(time.RFC3339),
		FixedCreateIntent: &intent,
	}
}

func (k FixedLeaseKind) FinalizeAfterCleanup(claim LeaseClaim, action func() error) error {
	if !k.IsFixedClaim(claim) {
		return RemoveLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, action)
	}
	tombstone := k.TerminalClaim(claim, time.Now().UTC())
	_, err := ReplaceLeaseClaimIfUnchangedDurableAfter(claim.LeaseID, claim, tombstone, action)
	return err
}

func (k FixedLeaseKind) ValidateTerminalClaim(claim, previous LeaseClaim, leaseID string, extra func(LeaseClaim) error) error {
	intent := claim.FixedCreateIntent
	if claim.LeaseID != leaseID || !k.IsFixedClaim(claim) ||
		intent.Version != k.IntentVersion ||
		strings.TrimSpace(intent.Fingerprint) == "" ||
		strings.TrimSpace(intent.ProviderScope) == "" ||
		claim.ProviderScope != intent.ProviderScope ||
		claim.Slug != intent.Slug ||
		intent.State != "released" ||
		claim.CloudID != "" || len(claim.Labels) != 0 || claim.SSHHost != "" || claim.SSHPort != 0 ||
		len(intent.Attempt) != 0 || len(intent.FailedAttempts) != 0 {
		return exit(4, "lease_id_conflict: fixed %s lease %s has an invalid terminal tombstone", k.Label, leaseID)
	}
	if extra != nil {
		if err := extra(claim); err != nil {
			return err
		}
	}
	if k.IsFixedClaim(previous) {
		previousIntent := previous.FixedCreateIntent
		if previous.LeaseID != leaseID ||
			previousIntent.Version != intent.Version ||
			previousIntent.Fingerprint != intent.Fingerprint ||
			previousIntent.ProviderScope != intent.ProviderScope ||
			previousIntent.CheckpointID != intent.CheckpointID ||
			previousIntent.Slug != intent.Slug {
			return exit(4, "lease_id_conflict: fixed %s lease %s terminal tombstone changed identity", k.Label, leaseID)
		}
	}
	return nil
}

func (k FixedLeaseKind) RetainClaimAfterRelease(
	leaseID string,
	previous LeaseClaim,
	providerEvidence bool,
	extraValidation func(LeaseClaim) error,
	afterValidation func(LeaseClaim) error,
) (bool, error) {
	claim, exists, err := ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return false, fmt.Errorf("re-attest released %s claim %s: %w", k.Label, leaseID, err)
	}
	if !exists || !k.IsFixedClaim(claim) {
		if k.IsFixedClaim(previous) || providerEvidence {
			return false, exit(4, "lease_id_conflict: fixed %s lease %s has no valid terminal tombstone after release", k.Label, leaseID)
		}
		return false, nil
	}
	if err := k.ValidateTerminalClaim(claim, previous, leaseID, extraValidation); err != nil {
		return false, err
	}
	if afterValidation != nil {
		if err := afterValidation(claim); err != nil {
			return false, err
		}
	}
	return true, nil
}
