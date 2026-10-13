package morph

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *morphLeaseBackend) ExtendLease(ctx context.Context, req core.LeaseExtendRequest) (core.LeaseExtendResult, error) {
	cfg := b.configForRun()
	return core.ExtendDirectLease(ctx, cfg, req, b.now().UTC(), func(ctx context.Context, server core.Server, _ core.LeaseClaim) error {
		client, err := b.api()
		if err != nil {
			return err
		}
		live, err := client.GetInstance(ctx, server.CloudID)
		if err != nil {
			return err
		}
		if !morphIsManaged(live) || live.Metadata["lease"] != req.Lease.LeaseID || live.Metadata["slug"] != server.Labels["slug"] {
			return core.Exit(4, "Morph lease identity changed")
		}
		if err := client.SetInstanceMetadata(ctx, live.ID, server.Labels); err != nil {
			return err
		}
		return client.UpdateInstanceTTL(ctx, live.ID, morphTTLSecondsFromLabels(server.Labels, b.now().UTC()), morphTTLAction(cfg))
	})
}
