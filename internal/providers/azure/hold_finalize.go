package azure

import (
	"context"
	"errors"
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

var _ core.FailedLeaseHoldFinalizerBackend = (*azureLeaseBackend)(nil)

// FinalizeFailedLeaseHold only verifies absence. Salvage and cloud disposal are
// separate operator actions; neither elapsed time nor absence proves salvage.
func (b *azureLeaseBackend) FinalizeFailedLeaseHold(ctx context.Context, id string) (receipt core.LeaseRecoveryHold, err error) {
	if !core.IsCanonicalLeaseID(id) {
		return receipt, core.Exit(2, "Azure hold finalization requires an exact canonical lease id")
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	client, err := newAzureClient(ctx, b.Cfg)
	if err != nil {
		return receipt, err
	}
	err = core.WithDurableLeaseClaimLockContext(ctx, id, func(claim *core.LeaseClaim, exists bool, persist func() error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !exists || claim.LeaseID != id || claim.Provider != "azure-recovery-held-v1" || claim.RecoveryHold == nil ||
			strings.TrimSpace(client.LeaseClaimScope()) == "" || claim.ProviderScope != client.LeaseClaimScope() ||
			claim.Slug == "" || claim.Slug != core.NormalizeLeaseSlug(claim.Slug) {
			return core.Exit(4, "Azure hold finalization requires the existing exact held claim and original account scope")
		}
		name := core.LeaseProviderName(id, claim.Slug)
		if claim.CloudID != name && (claim.CloudID != "" || claim.CloudImmutableID != "" || claim.FixedCreateIntent == nil) {
			return core.Exit(4, "Azure held claim resource identity changed")
		}
		if intent := claim.FixedCreateIntent; intent != nil {
			if intent.Version != fixedAzureLeaseKind.IntentVersion || intent.State != "held" ||
				intent.ProviderScope != claim.ProviderScope || intent.Slug != claim.Slug || intent.Attempt["name"] != name ||
				intent.Attempt["nonce"] == "" || !core.FixedSHA256(intent.Fingerprint) ||
				claim.Labels["lease"] != id || claim.Labels["slug"] != claim.Slug ||
				claim.Labels["fixed_attempt"] != intent.Attempt["nonce"] || claim.Labels["fixed_intent_sha256"] != intent.Fingerprint {
				return core.Exit(4, "Azure held claim fixed identity changed")
			}
		} else if claim.CloudImmutableID != "" {
			return core.Exit(4, "Azure held claim is missing its original fixed intent")
		}
		held := *claim.RecoveryHold
		if err := validateAzureHoldFinalizationReceipt(held, id, claim.Slug, claim.ProviderScope, held.Status); err != nil {
			return err
		}
		if held.Status == "finalized" {
			receipt = held
			return nil
		}
		verifier, ok := client.(interface {
			VerifyFailedLeaseHoldAbsent(context.Context, string, string) (core.LeaseRecoveryHold, error)
		})
		if !ok {
			return core.Exit(2, "Azure client cannot verify failed-lease hold absence")
		}
		observed, err := verifier.VerifyFailedLeaseHoldAbsent(ctx, id, claim.Slug)
		if err != nil {
			return err
		}
		if err := validateAzureHoldFinalizationReceipt(observed, id, claim.Slug, claim.ProviderScope, "finalized"); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		original := *claim
		claim.RecoveryHold = &observed
		// Keep the held-provider marker and original intent: old writers must
		// reject the ID even if they ignore the additive terminal receipt.
		if err := persist(); err != nil {
			// A directory-sync failure can follow the atomic rename. Restore
			// custody before returning; even if restoration fails, both
			// records keep the protective provider marker and single-use ID.
			*claim = original
			return errors.Join(err, persist())
		}
		receipt = observed
		return nil
	})
	return receipt, err
}

// Validate both persisted custody and the verifier's complete ordered receipt.
// Exact slots prevent a malformed receipt from omitting or substituting a disk.
func validateAzureHoldFinalizationReceipt(receipt core.LeaseRecoveryHold, id, slug, scope, status string) error {
	invalid := func() error {
		return core.Exit(4, "Azure hold receipt is incomplete or does not match the exact held claim")
	}
	if (status != "held" && status != "finalized") || receipt.Schema != "crabbox.lease-hold.v1" ||
		receipt.Provider != "azure" || receipt.LeaseID != id || receipt.Status != status || receipt.UnacceptedChanges != "unknown" {
		return invalid()
	}
	subscription, group, ok := strings.Cut(strings.TrimPrefix(scope, "subscription:"), "|resource-group:")
	if !ok || !strings.HasPrefix(scope, "subscription:") || subscription == "" || group == "" ||
		strings.ContainsAny(subscription+group, "/|\\") {
		return invalid()
	}
	name := core.LeaseProviderName(id, slug)
	slots := []struct{ kind, path, suffix string }{
		{"vm", "Microsoft.Compute/virtualMachines", ""},
		{"nic", "Microsoft.Network/networkInterfaces", "-nic"},
		{"public-ip", "Microsoft.Network/publicIPAddresses", "-pip"},
		{"disk", "Microsoft.Compute/disks", "-osdisk"},
		{"nsg", "Microsoft.Network/networkSecurityGroups", "-q-nsg"},
	}
	if len(receipt.Resources) != len(slots) {
		return invalid()
	}
	for i, slot := range slots {
		resource := receipt.Resources[i]
		want := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/%s/%s%s", subscription, group, slot.path, name, slot.suffix)
		if resource.Kind != slot.kind || !strings.EqualFold(resource.ID, want) ||
			(resource.State != "absent" && (status != "held" || i == 0 || resource.State != "retained" || strings.TrimSpace(resource.ImmutableID) == "")) {
			return invalid()
		}
	}
	return nil
}
