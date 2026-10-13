package azure

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *azureLeaseBackend) ExtendLease(ctx context.Context, req core.LeaseExtendRequest) (core.LeaseExtendResult, error) {
	return core.ExtendDirectLease(ctx, b, b.Cfg, req, core.ClockNow(b.RT.Clock).UTC(), func(ctx context.Context, server core.Server, claim core.LeaseClaim) error {
		client, err := newAzureClient(ctx, b.Cfg)
		if err != nil {
			return err
		}
		live, err := client.GetServer(ctx, server.CloudID)
		if err != nil {
			return err
		}
		if err := core.ValidateAzureOwnedVM(server, live); err != nil {
			return err
		}
		if err := validateExactAzureClaim(claim, live, req.Lease.LeaseID, client.LeaseClaimScope()); err != nil {
			return err
		}
		return client.SetTags(ctx, server.CloudID, server.Labels)
	})
}
