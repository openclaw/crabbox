package hetzner

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func (b *hetznerLeaseBackend) UpdateTailscaleMetadata(ctx context.Context, lease core.LeaseTarget, meta core.TailscaleMetadata) (core.Server, error) {
	return shared.PersistTailscaleMetadata(ctx, lease, meta, func(claim core.LeaseClaim, apply func(*core.Server)) (core.Server, error) {
		client, err := newHetznerClient()
		if err != nil {
			return core.Server{}, err
		}
		server, err := client.GetServer(ctx, lease.Server.ID)
		if err != nil {
			return core.Server{}, err
		}
		server = normalizeHetznerServer(server)
		if err := validateHetznerClaim(claim, server, lease.LeaseID); err != nil {
			return core.Server{}, err
		}
		if err := validateHetznerServerOwnership(server, true); err != nil {
			return core.Server{}, err
		}
		apply(&server)
		return server, client.SetLabels(ctx, server.ID, server.Labels)
	})
}
