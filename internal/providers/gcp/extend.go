package gcp

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *gcpLeaseBackend) ExtendLease(ctx context.Context, req core.LeaseExtendRequest) (core.LeaseExtendResult, error) {
	return core.ExtendDirectLease(ctx, b, b.Cfg, req, core.ClockNow(b.RT.Clock).UTC(), func(ctx context.Context, server core.Server, claim core.LeaseClaim) error {
		cfg := b.Cfg
		if zone := server.Labels["zone"]; zone != "" {
			cfg.GCP.Zone = zone
		}
		client, err := newGCPClient(ctx, cfg)
		if err != nil {
			return err
		}
		live, err := client.GetServer(ctx, server.CloudID)
		if err != nil {
			return err
		}
		if live.Labels["lease"] != req.Lease.LeaseID || live.Labels["slug"] != server.Labels["slug"] {
			return core.Exit(4, "GCP lease identity changed; refusing extension")
		}
		return client.SetLabels(ctx, server.CloudID, server.Labels)
	})
}
