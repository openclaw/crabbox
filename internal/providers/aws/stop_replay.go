package aws

import (
	"context"
	"encoding/hex"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func finalizeAWSLeaseAfterCleanup(expected core.LeaseClaim, action func() error) error {
	if !fixedAWSLeaseKind.IsFixedClaim(expected) {
		return core.RemoveLeaseClaimIfUnchangedAfter(expected.LeaseID, expected, action)
	}
	tombstone := fixedAWSLeaseKind.TerminalClaim(expected, time.Now().UTC())
	return core.WithDurableLeaseClaimLock(expected.LeaseID, func(claim *core.LeaseClaim, exists bool, persist func() error) error {
		if !exists || !reflect.DeepEqual(*claim, expected) {
			return exit(2, "lease %s claim changed; retry", expected.LeaseID)
		}
		if err := action(); err != nil {
			return err
		}
		*claim = tombstone
		if err := persist(); err != nil {
			return err
		}
		// Save remote completion before fallible local cleanup so stop can retry.
		// Keep the claim fence until cleanup ends; replacement owners keep their keys.
		return cleanupAWSLeaseSSH(expected.LeaseID)
	})
}

func cleanupAWSLeaseSSH(leaseID string) error {
	if err := core.RemoveStoredTestboxConnectionArtifacts(leaseID); err != nil {
		return fmt.Errorf("AWS resources released for lease %s; local SSH cleanup failed: %w; retry crabbox stop --provider aws --id %s", leaseID, err, leaseID)
	}
	return nil
}

// A terminal claim is local cleanup evidence, not a provider status word.
// Historical compact tombstones and no-allocation rejections lack this binding.
func validateAWSTerminalReceipt(claim core.LeaseClaim, leaseID string) error {
	if err := fixedAWSLeaseKind.ValidateTerminalClaim(claim, core.LeaseClaim{}, leaseID, nil); err != nil {
		return err
	}
	intent := claim.FixedCreateIntent
	labels := claim.Labels
	account := labels["aws_account_id"]
	digest, digestErr := hex.DecodeString(intent.Fingerprint)
	_, timeErr := time.Parse(time.RFC3339Nano, intent.CreatedAt)
	if !core.IsCanonicalLeaseID(leaseID) ||
		!strings.HasPrefix(claim.CloudID, "i-") || len(claim.CloudID) <= 2 || strings.TrimSpace(claim.CloudID) != claim.CloudID ||
		claim.Slug == "" || core.NormalizeLeaseSlug(claim.Slug) != claim.Slug ||
		!isCrabboxAWSLease(Server{Labels: labels}) || labels["lease"] != leaseID || labels["slug"] != claim.Slug ||
		len(account) != 12 || strings.Trim(account, "0123456789") != "" || claim.ProviderScope != "account:"+account ||
		labels["aws_region"] == "" || strings.TrimSpace(labels["aws_region"]) != labels["aws_region"] ||
		digestErr != nil || len(digest) != 32 || strings.ToLower(intent.Fingerprint) != intent.Fingerprint ||
		labels["fixed_intent_sha256"] != intent.Fingerprint || timeErr != nil ||
		labels["provider_key"] != core.ProviderKeyForLease(leaseID) || strings.TrimSpace(labels["aws_key_pair_id"]) == "" {
		return exit(4, "lease_id_conflict: fixed AWS lease %s lacks a valid scoped terminal receipt; compact historical tombstones cannot prove cleanup scope", leaseID)
	}
	return nil
}

func (b *awsLeaseBackend) validateTerminalReleaseScope(ctx context.Context, claim core.LeaseClaim, leaseID string) error {
	if err := validateAWSTerminalReceipt(claim, leaseID); err != nil {
		return err
	}
	// Only the request's configured regions authorize scope; a receipt must not
	// silently expand that scope by selecting its own region.
	for _, cfg := range awsRegionConfigs(b.Cfg) {
		if cfg.AWSRegion != claim.Labels["aws_region"] {
			continue
		}
		client, err := newAWSClient(ctx, cfg)
		if err != nil {
			return err
		}
		matches, err := awsClaimMatchesCurrentAccount(ctx, client, claim)
		if err != nil {
			return fmt.Errorf("verify AWS terminal receipt account: %w", err)
		}
		if !matches {
			return exit(4, "lease_id_conflict: fixed AWS lease %s terminal receipt belongs to another AWS account", leaseID)
		}
		return nil
	}
	return exit(4, "lease_id_conflict: fixed AWS lease %s terminal receipt is outside configured AWS regions", leaseID)
}

func validateAWSTerminalInventory(claim core.LeaseClaim, servers []Server) error {
	for _, server := range servers {
		// Even incomplete or conflicting ownership tags must not conceal a
		// visible resource behind the receipt. Do not filter by provider status.
		if server.CloudID == claim.CloudID || server.Labels["lease"] == claim.LeaseID {
			return exit(4, "lease_id_conflict: fixed AWS lease %s terminal receipt conflicts with visible instance %s", claim.LeaseID, server.DisplayID())
		}
	}
	return nil
}

func awsTerminalReleaseTarget(claim core.LeaseClaim) LeaseTarget {
	server := Server{
		Provider: "aws", CloudID: claim.CloudID, ImmutableID: claim.CloudImmutableID,
		Name: claim.Slug, Status: fixedAWSIntentReleased, Labels: maps.Clone(claim.Labels),
	}
	core.SetServerLeaseClaimSnapshot(&server, claim, true)
	return LeaseTarget{LeaseID: claim.LeaseID, Server: server}
}

func (b *awsLeaseBackend) resolveTerminalRelease(ctx context.Context, req ResolveRequest, claim core.LeaseClaim, servers []Server) (LeaseTarget, error) {
	if err := b.validateTerminalReleaseScope(ctx, claim, req.ID); err != nil {
		return LeaseTarget{}, err
	}
	if err := validateAWSTerminalInventory(claim, servers); err != nil {
		return LeaseTarget{}, err
	}
	lease := awsTerminalReleaseTarget(claim)
	if err := core.ValidateLeaseTargetProviderIdentity(lease, req.ExpectedProviderIdentity); err != nil {
		return LeaseTarget{}, err
	}
	return lease, nil
}

func (b *awsLeaseBackend) releaseTerminalReceipt(ctx context.Context, req ReleaseLeaseRequest, outcome *core.ReleaseLeaseOutcome) error {
	snapshot, snapshotExists, snapshotSet := core.ServerLeaseClaimSnapshot(req.Lease.Server)
	return core.WithDurableLeaseClaimLock(req.Lease.LeaseID, func(claim *core.LeaseClaim, exists bool, _ func() error) error {
		if !exists || !snapshotSet || !snapshotExists || !reflect.DeepEqual(snapshot, *claim) {
			return exit(4, "lease_id_conflict: fixed AWS lease %s terminal receipt changed or has no resolved durable snapshot", req.Lease.LeaseID)
		}
		if err := b.validateTerminalReleaseScope(ctx, *claim, req.Lease.LeaseID); err != nil {
			return err
		}
		if !reflect.DeepEqual(req.Lease, awsTerminalReleaseTarget(*claim)) {
			return exit(4, "lease_id_conflict: fixed AWS lease %s terminal target differs from its durable receipt", req.Lease.LeaseID)
		}
		if err := core.ValidateLeaseTargetProviderIdentity(req.Lease, req.ExpectedProviderIdentity); err != nil {
			return err
		}
		// Refresh inventory under the same fence as the durable evidence. Resolve
		// alone cannot authorize success across a new visible-resource conflict.
		servers, err := b.listAcrossRegions(ctx)
		if err != nil {
			return err
		}
		if err := validateAWSTerminalInventory(*claim, servers); err != nil {
			return err
		}
		outcome.Terminal = true
		return cleanupAWSLeaseSSH(claim.LeaseID)
	})
}
