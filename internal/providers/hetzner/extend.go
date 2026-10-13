package hetzner

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *hetznerLeaseBackend) ExtendLease(ctx context.Context, req core.LeaseExtendRequest) (core.LeaseExtendResult, error) {
	return core.ExtendDirectLease(ctx, b, b.Cfg, req, core.ClockNow(b.RT.Clock).UTC(), func(ctx context.Context, server core.Server, claim core.LeaseClaim) error {
		client, err := newHetznerClient()
		if err != nil {
			return err
		}
		live, err := client.GetServer(ctx, server.ID)
		if err != nil {
			return err
		}
		if err := validateHetznerServerOwnership(live, false); err != nil {
			return err
		}
		if live.Labels["lease"] != req.Lease.LeaseID || live.Labels["slug"] != server.Labels["slug"] {
			return core.Exit(4, "Hetzner lease identity changed; refusing extension")
		}
		return client.SetLabels(ctx, server.ID, server.Labels)
	})
}
