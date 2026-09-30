package azure

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *azureLeaseBackend) HoldFailedLease(ctx context.Context, id string) (receipt core.LeaseRecoveryHold, err error) {
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
		if !exists || claim.ProviderScope != client.LeaseClaimScope() || claim.FixedCreateIntent == nil {
			return core.Exit(4, "Azure hold requires the exact fixed claim and account scope")
		}
		if claim.RecoveryHold != nil {
			if claim.Provider != "azure-recovery-held-v1" || claim.RecoveryHold.LeaseID != id || claim.RecoveryHold.Provider != "azure" {
				return core.Exit(4, "Azure held claim identity changed")
			}
			receipt = *claim.RecoveryHold
			return nil
		}
		expected := azureServerFromClaim(*claim)
		if err := validateExactAzureClaim(*claim, expected, id, client.LeaseClaimScope()); err != nil {
			return err
		}
		observed, err := inspector.InspectFailedLeaseHold(ctx, expected)
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
