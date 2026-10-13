package incus

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) ExtendLease(ctx context.Context, req core.LeaseExtendRequest) (core.LeaseExtendResult, error) {
	cfg := b.configForRun()
	return core.ExtendDirectLease(ctx, b, cfg, req, core.ClockNow(b.rt.Clock).UTC(), func(ctx context.Context, server core.Server, _ core.LeaseClaim) error {
		client, err := newClient(cfg)
		if err != nil {
			return err
		}
		inst, _, err := client.GetInstance(server.CloudID)
		if err != nil {
			return err
		}
		if req.Lease.Server.ImmutableID != "" && inst.Config["volatile.uuid"] != req.Lease.Server.ImmutableID {
			return core.Exit(4, "Incus resource identity changed")
		}
		if err := validateResolvedInstance(client, *inst, req.Lease.LeaseID); err != nil {
			return err
		}
		return setInstanceLabels(ctx, client, server.CloudID, server.Labels)
	})
}
