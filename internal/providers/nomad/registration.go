package nomad

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	registrationVersionLabel  = "nomad_registration_version"
	registrationStateLabel    = "nomad_registration_state"
	registrationMetaKeysLabel = "nomad_registration_metadata_keys"
	registrationMetaHashLabel = "nomad_registration_metadata_sha256"
	registrationEvalLabel     = "nomad_registration_evaluation"
	registrationPrepared      = "prepared"
	registrationSubmitting    = "submitting"
	registrationConfirmed     = "confirmed"
)

type preparedRegistration struct {
	job   *nomadapi.Job
	claim LeaseClaim
}

func registrationState(claim LeaseClaim) (string, error) {
	version, hasVersion := claim.Labels[registrationVersionLabel]
	state, hasState := claim.Labels[registrationStateLabel]
	if !hasVersion && !hasState {
		if _, exists := claim.Labels[registrationMetaKeysLabel]; exists {
			return "", exit(4, "nomad lease %s has incomplete registration markers", claim.LeaseID)
		}
		if _, exists := claim.Labels[registrationMetaHashLabel]; exists {
			return "", exit(4, "nomad lease %s has incomplete registration markers", claim.LeaseID)
		}
		if _, exists := claim.Labels[registrationEvalLabel]; exists {
			return "", exit(4, "nomad lease %s has incomplete registration markers", claim.LeaseID)
		}
		return registrationConfirmed, nil // Legacy claims were published after registration.
	}
	if version != "1" {
		return "", exit(4, "nomad lease %s has an unsupported registration version", claim.LeaseID)
	}
	if _, _, err := registrationMetadataFingerprint(claim); err != nil {
		return "", err
	}
	switch state {
	case registrationPrepared, registrationSubmitting, registrationConfirmed:
		return state, nil
	default:
		return "", exit(4, "nomad lease %s has an invalid registration state", claim.LeaseID)
	}
}

func unresolvedRegistrationError(claim LeaseClaim) error {
	return exit(5, "nomad lease %s job %s registration outcome is unknown; claim retained", claim.LeaseID, claim.Labels[claimLabelJobID])
}

func registrationRecovery(ctx context.Context, claim LeaseClaim) (*shared.DelegatedSandboxRecovery, error) {
	if claim.LeaseID == "" || claim.Provider != providerName || claim.Labels[claimLabelJobID] == "" {
		return nil, exit(4, "nomad registration recovery has no bound claim")
	}
	if _, err := registrationState(claim); err != nil {
		return nil, err
	}
	if err := core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, claim, func() error { return nil }); err != nil {
		return nil, err
	}
	return &shared.DelegatedSandboxRecovery{
		LeaseID: claim.LeaseID, Slug: claim.Slug,
		CleanupCommand: "crabbox stop --provider nomad " + shellQuote(claim.LeaseID),
	}, nil
}

func registrationRecoveryError(claim LeaseClaim, cause error) error {
	message := fmt.Sprintf("nomad registration lease=%s slug=%s job=%s scope=%q: %v; inspect with crabbox status --provider nomad --id %s and recover with crabbox stop --provider nomad %s", claim.LeaseID, claim.Slug, claim.Labels[claimLabelJobID], claim.ProviderScope, cause, claim.LeaseID, claim.LeaseID)
	return shared.ExitErrorWithCause(core.ExitCodeForError(cause, 1), message, cause)
}

func (b *backend) prepareRegistration(ctx context.Context, repo Repo, requestedSlug string) (*preparedRegistration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	leaseID, err := newLeaseID()
	if err != nil {
		return nil, err
	}
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return nil, err
	}
	expiresAt := time.Time{}
	if b.cfg.TTL > 0 {
		expiresAt = core.ClockNow(b.rt.Clock).UTC().Add(b.cfg.TTL)
	}
	jobID := jobIDForLease(leaseID)
	job, err := buildJobSpec(b.cfg, jobSpecInput{LeaseID: leaseID, Slug: slug, JobID: jobID, ExpiresAt: expiresAt})
	if err != nil {
		return nil, err
	}
	metadata, err := json.Marshal(job.Meta)
	if err != nil {
		return nil, err
	}
	labels := claimLabels(b.cfg, leaseID, slug, allocationReadiness{JobID: jobID}, expiresAt)
	labels[registrationVersionLabel] = "1"
	labels[registrationStateLabel] = registrationPrepared
	keys := make([]string, 0, len(job.Meta))
	for key := range job.Meta {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	keyJSON, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(metadata)
	labels[registrationMetaKeysLabel] = string(keyJSON)
	labels[registrationMetaHashLabel] = hex.EncodeToString(digest[:])
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	claim, err := core.ClaimLeaseForRepoProviderScopePondWithLabels(leaseID, slug, providerName, claimScope(b.cfg), b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, labels)
	if err != nil {
		// Admission may be visible after a durability error; never submit after it.
		return nil, registrationRecoveryError(LeaseClaim{LeaseID: leaseID, Slug: slug, ProviderScope: claimScope(b.cfg), Labels: labels}, err)
	}
	return &preparedRegistration{job: job, claim: claim}, nil
}

func advanceRegistration(ctx context.Context, claim *LeaseClaim, state, evaluation string, ready *allocationReadiness) error {
	previous, err := registrationState(*claim)
	if err != nil {
		return err
	}
	if claim.Labels[registrationVersionLabel] != "1" ||
		(previous == registrationPrepared && state != registrationSubmitting) ||
		(previous != registrationPrepared && state != registrationConfirmed) {
		return exit(4, "nomad lease %s has an invalid registration transition", claim.LeaseID)
	}
	next := *claim
	next.Labels = maps.Clone(claim.Labels)
	next.Labels[registrationStateLabel] = state
	if evaluation != "" {
		next.Labels[registrationEvalLabel] = evaluation
	}
	if ready != nil {
		applyReadinessLabels(next.Labels, *ready)
		next.Labels[claimLabelState] = ready.State()
		// Match the old ready-claim publication point, not preparation time.
		next.ClaimedAt = time.Now().UTC().Format(time.RFC3339)
		next.LastUsedAt = next.ClaimedAt
	}
	updated, err := core.ReplaceLeaseClaimIfUnchangedDurableReturningContext(ctx, claim.LeaseID, *claim, next)
	if err != nil {
		return err
	}
	*claim = updated
	return nil
}

func (b *backend) submitRegistration(ctx context.Context, client Client, prepared *preparedRegistration) (allocationReadiness, error) {
	state, err := registrationState(prepared.claim)
	if err != nil {
		return allocationReadiness{}, err
	}
	if state != registrationPrepared {
		return allocationReadiness{}, exit(4, "nomad lease %s registration was already submitted", prepared.claim.LeaseID)
	}
	if err := ctx.Err(); err != nil {
		return allocationReadiness{}, err
	}
	if err := advanceRegistration(ctx, &prepared.claim, registrationSubmitting, "", nil); err != nil {
		return allocationReadiness{}, err
	}
	if err := ctx.Err(); err != nil {
		return allocationReadiness{}, err
	}
	evalID, err := client.RegisterJob(ctx, prepared.job)
	if err != nil {
		return allocationReadiness{}, err
	}
	// Acknowledged submission is distinct from allocation readiness.
	recordCtx, cancel := b.cleanupContext(ctx)
	err = advanceRegistration(recordCtx, &prepared.claim, registrationConfirmed, evalID, nil)
	cancel()
	if err != nil {
		return allocationReadiness{}, err
	}
	if evalID != "" {
		if err := b.waitForEvaluation(ctx, client, evalID); err != nil {
			return allocationReadiness{}, err
		}
	}
	ready, err := b.waitForAllocation(ctx, client, prepared.claim.Labels[claimLabelJobID], b.allocReadyTimeout())
	if err != nil {
		return allocationReadiness{}, err
	}
	if err := advanceRegistration(ctx, &prepared.claim, registrationConfirmed, "", &ready); err != nil {
		return allocationReadiness{}, err
	}
	return ready, nil
}

func validateRegistrationMetadata(claim LeaseClaim, job *nomadapi.Job) error {
	if !registrationAwaitingReadiness(claim) {
		return nil
	}
	keys, expectedHash, err := registrationMetadataFingerprint(claim)
	if err != nil {
		return err
	}
	if job == nil || stringValue(job.ID) != claim.Labels[claimLabelJobID] {
		return exit(4, "nomad job for lease %s registration ownership changed", claim.LeaseID)
	}
	selected := make(map[string]string, len(keys))
	for _, key := range keys {
		// Match metadataMatches: added keys are ignored; missing empty values match.
		selected[key] = strings.TrimSpace(job.Meta[key])
	}
	data, err := json.Marshal(selected)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != expectedHash {
		return exit(4, "nomad job for lease %s registration ownership changed", claim.LeaseID)
	}
	return nil
}

// Allocation ID is committed only after successful readiness. Normal ready and
// legacy claims retain their existing reserved-ownership metadata contract.
func registrationAwaitingReadiness(claim LeaseClaim) bool {
	return claim.Labels[registrationVersionLabel] != "" && claim.Labels[claimLabelAllocationID] == ""
}

func registrationMetadataFingerprint(claim LeaseClaim) ([]string, string, error) {
	var keys []string
	hash := claim.Labels[registrationMetaHashLabel]
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != sha256.Size || json.Unmarshal([]byte(claim.Labels[registrationMetaKeysLabel]), &keys) != nil || len(keys) == 0 || !sort.StringsAreSorted(keys) {
		return nil, "", exit(4, "nomad lease %s has invalid registration metadata fingerprint", claim.LeaseID)
	}
	for i := 1; i < len(keys); i++ {
		if keys[i] == keys[i-1] {
			return nil, "", exit(4, "nomad lease %s has duplicate registration metadata keys", claim.LeaseID)
		}
	}
	return keys, hash, nil
}
