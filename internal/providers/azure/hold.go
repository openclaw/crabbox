package azure

import (
	"context"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *azureLeaseBackend) HoldFailedLease(ctx context.Context, id string) (receipt core.LeaseRecoveryHold, err error) {
	return b.holdFailedLease(ctx, id, "")
}

func (b *azureLeaseBackend) HoldFailedLeaseWithSlug(ctx context.Context, id, slug string) (receipt core.LeaseRecoveryHold, err error) {
	if slug == "" || slug != core.NormalizeLeaseSlug(slug) {
		return receipt, core.Exit(2, "Azure claimless hold requires the original exact lease slug")
	}
	return b.holdFailedLease(ctx, id, slug)
}

func (b *azureLeaseBackend) holdFailedLease(ctx context.Context, id, slug string) (receipt core.LeaseRecoveryHold, err error) {
	if !core.IsCanonicalLeaseID(id) {
		return receipt, core.Exit(2, "Azure hold requires an exact canonical lease id")
	}
	client, err := newAzureClient(ctx, b.Cfg)
	if err != nil {
		return receipt, err
	}
	inspector, ok := client.(interface {
		InspectFailedLeaseHold(context.Context, core.Server) (core.LeaseRecoveryHold, error)
	})
	if !ok {
		return receipt, core.Exit(2, "Azure client cannot attest a failed lease hold")
	}
	err = core.WithDurableLeaseClaimLockContext(ctx, id, func(claim *core.LeaseClaim, exists bool, persist func() error) error {
		if exists && claim.RecoveryHold != nil {
			if claim.ProviderScope != client.LeaseClaimScope() || (slug != "" && claim.Slug != slug) {
				return core.Exit(4, "Azure held claim account or slug changed")
			}
			if claim.Provider != "azure-recovery-held-v1" || claim.RecoveryHold.LeaseID != id || claim.RecoveryHold.Provider != "azure" {
				return core.Exit(4, "Azure held claim identity changed")
			}
			receipt = *claim.RecoveryHold
			return nil
		}
		if !exists {
			if slug == "" || strings.TrimSpace(client.LeaseClaimScope()) == "" {
				return core.Exit(4, "Azure claimless hold requires an original slug and account scope")
			}
			observer, ok := client.(interface {
				InspectClaimlessFailedLeaseHold(context.Context, string, string) (core.LeaseRecoveryHold, error)
			})
			if !ok {
				return core.Exit(2, "Azure client cannot attest a claimless failed lease hold")
			}
			name := core.LeaseProviderName(id, slug)
			candidate := core.LeaseClaim{LeaseID: id, Slug: slug, Provider: "azure-recovery-held-v1",
				ProviderScope: client.LeaseClaimScope(), CloudID: name}
			if err := core.ValidateFixedLocalClaimUniqueness(fixedAzureLeaseKind, candidate, "azure", "azure-recovery-held-v1"); err != nil {
				return err
			}
			observed, err := observer.InspectClaimlessFailedLeaseHold(ctx, id, slug)
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			candidate.RecoveryHold = &observed
			*claim = candidate
			if err := persist(); err != nil {
				return err
			}
			receipt = observed
			return nil
		}
		if claim.ProviderScope != client.LeaseClaimScope() || claim.FixedCreateIntent == nil || (slug != "" && claim.Slug != slug) {
			return core.Exit(4, "Azure hold requires the exact fixed claim and account scope")
		}
		expected := azureServerFromClaim(*claim)
		var observed core.LeaseRecoveryHold
		if claim.CloudID == "" && claim.CloudImmutableID == "" {
			intent := claim.FixedCreateIntent
			if !fixedAzureLeaseKind.IsFixedClaim(*claim) || intent.Version != fixedAzureLeaseKind.IntentVersion ||
				intent.State != "prepared" || intent.ProviderScope != claim.ProviderScope || intent.Slug != claim.Slug ||
				claim.LeaseID != id || claim.Labels["lease"] != id || claim.Labels["slug"] != claim.Slug ||
				claim.Labels["fixed_attempt"] != intent.Attempt["nonce"] || claim.Labels["fixed_intent_sha256"] != intent.Fingerprint ||
				intent.Attempt["name"] != core.LeaseProviderName(id, claim.Slug) {
				return core.Exit(4, "Azure hold requires the original unbound fixed attempt")
			}
			observer, ok := client.(interface {
				InspectUnboundFailedLeaseHold(context.Context, core.Server, core.AzureFixedCompanions) (core.LeaseRecoveryHold, error)
			})
			if !ok {
				return core.Exit(2, "Azure client cannot attest an unbound failed lease hold")
			}
			expected.CloudID, expected.Name = intent.Attempt["name"], intent.Attempt["name"]
			observed, err = observer.InspectUnboundFailedLeaseHold(ctx, expected, core.AzureFixedCompanions{
				NICGUID: intent.Attempt["pre_vm_nic_guid"], PublicIPGUID: intent.Attempt["pre_vm_public_ip_guid"],
			})
		} else {
			if err := validateExactAzureClaim(*claim, expected, id, client.LeaseClaimScope()); err != nil {
				return err
			}
			observed, err = inspector.InspectFailedLeaseHold(ctx, expected)
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		claim.RecoveryHold = &observed
		// Older Azure writers reject this provider identity instead of ignoring the additive hold.
		claim.Provider = "azure-recovery-held-v1"
		claim.FixedCreateIntent.State = "held"
		if err := persist(); err != nil {
			return err
		}
		receipt = observed
		return nil
	})
	return receipt, err
}
