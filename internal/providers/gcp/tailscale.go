package gcp

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func (b *gcpLeaseBackend) UpdateTailscaleMetadata(ctx context.Context, lease core.LeaseTarget, meta core.TailscaleMetadata) (core.Server, error) {
	cfg := b.Cfg
	if zone := lease.Server.Labels["zone"]; zone != "" {
		cfg.GCP.Zone = zone
	}
	return shared.PersistTailscaleMetadata(ctx, lease, meta, func(claim core.LeaseClaim, apply func(*core.Server)) (core.Server, error) {
		client, err := newGCPClient(ctx, cfg)
		if err != nil {
			return core.Server{}, err
		}
		server, err := client.GetServer(ctx, lease.Server.CloudID)
		if err != nil {
			return core.Server{}, err
		}
		if err := validateExactGCPClaim(claim, server, lease.LeaseID, cfg); err != nil {
			return core.Server{}, err
		}
		apply(&server)
		return server, client.SetLabels(ctx, server.CloudID, server.Labels)
	})
}
