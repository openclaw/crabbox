package morph

import (
	"context"
	"fmt"
	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *morphLeaseBackend) ExtendLease(ctx context.Context, req core.LeaseExtendRequest) (core.LeaseExtendResult, error) {
	cfg := b.configForRun()
	return core.ExtendDirectLease(ctx, b, cfg, req, b.now().UTC(), func(ctx context.Context, server core.Server, _ core.LeaseClaim) error {
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
		// Publish the total TTL only after the native deadline accepts it, so an
		// additive retry still derives the same lifetime from the old metadata.
		if err := client.UpdateInstanceTTL(ctx, live.ID, morphTTLSecondsFromLabels(server.Labels, b.now().UTC()), morphTTLAction(cfg)); err != nil {
			return err
		}
		if err := client.SetInstanceMetadata(ctx, live.ID, server.Labels); err != nil {
			return fmt.Errorf("Morph native deadline extended to %s, but provider metadata update failed; rerun the same extension to retry: %w", core.LeaseLabelTimeDisplay(server.Labels["expires_at"]), err)
		}
		return nil
	})
}
