package proxmox

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *leaseBackend) ExtendLease(ctx context.Context, req core.LeaseExtendRequest) (core.LeaseExtendResult, error) {
	return core.ExtendDirectLease(ctx, b, b.Cfg, req, core.ClockNow(b.RT.Clock).UTC(), func(ctx context.Context, server core.Server, _ core.LeaseClaim) error {
		client, err := newClient(b.Cfg)
		if err != nil {
			return err
		}
		node := core.Blank(server.HostID, b.Cfg.Proxmox.Node)
		live, err := client.GetServerOnNode(ctx, node, server.CloudID)
		if err != nil {
			return err
		}
		if live.Labels["lease"] != req.Lease.LeaseID || live.Labels["slug"] != server.Labels["slug"] {
			return core.Exit(4, "Proxmox lease identity changed; refusing extension")
		}
		return client.SetLabelsOnNode(ctx, node, server.CloudID, server.Labels)
	})
}
