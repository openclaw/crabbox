package nebius

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) ExtendLease(ctx context.Context, req core.LeaseExtendRequest) (core.LeaseExtendResult, error) {
	return core.ExtendDirectLease(ctx, b, b.Cfg, req, core.ClockNow(b.RT.Clock).UTC(), func(ctx context.Context, server core.Server, claim core.LeaseClaim) error {
		client := b.clientFactory(b.RT)
		live, err := client.GetInstance(ctx, server.CloudID)
		if err != nil {
			return err
		}
		observed := serverFromInstance(live, b.Cfg)
		if err := validateNebiusOwnership(observed.Labels, b.Cfg); err != nil {
			return err
		}
		if observed.Labels["lease"] != req.Lease.LeaseID || observed.Labels["slug"] != server.Labels["slug"] {
			return core.Exit(4, "Nebius lease identity changed; refusing extension")
		}
		return client.UpdateLabels(ctx, server.CloudID, server.Labels)
	})
}
