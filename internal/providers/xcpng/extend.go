package xcpng

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *leaseBackend) ExtendLease(ctx context.Context, req core.LeaseExtendRequest) (core.LeaseExtendResult, error) {
	return core.ExtendDirectLease(ctx, b, b.Cfg, req, core.ClockNow(b.RT.Clock).UTC(), func(ctx context.Context, server core.Server, _ core.LeaseClaim) error {
		client, err := newLifecycleClient(ctx, b.Cfg)
		if err != nil {
			return err
		}
		defer closeClient(ctx, client, b.RT.Stderr)
		live, err := client.GetServer(ctx, server.CloudID)
		if err != nil {
			return err
		}
		if !isCrabboxLease(live) || live.Labels["lease"] != req.Lease.LeaseID || live.Labels["slug"] != server.Labels["slug"] {
			return core.Exit(4, "XCP-ng lease identity changed; refusing extension")
		}
		return client.SetLabels(ctx, server.CloudID, server.Labels)
	})
}
