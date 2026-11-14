package azure

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func (b *azureLeaseBackend) UpdateTailscaleMetadata(ctx context.Context, lease core.LeaseTarget, meta core.TailscaleMetadata) (core.Server, error) {
	return shared.PersistTailscaleMetadata(ctx, lease, meta, func(claim core.LeaseClaim, apply func(*core.Server)) (core.Server, error) {
		client, err := newAzureClient(ctx, b.Cfg)
		if err != nil {
			return core.Server{}, err
		}
		server, err := client.GetServer(ctx, lease.Server.CloudID)
		if err != nil {
			return core.Server{}, err
		}
		if err := validateExactAzureClaim(claim, server, lease.LeaseID, client.LeaseClaimScope()); err != nil {
			return core.Server{}, err
		}
		apply(&server)
		return server, client.SetTags(ctx, server.CloudID, server.Labels)
	})
}
