package tencentcloud

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *Backend) ExtendLease(ctx context.Context, req core.LeaseExtendRequest) (core.LeaseExtendResult, error) {
	return core.ExtendDirectLease(ctx, b, b.Cfg, req, b.clockNow(), func(ctx context.Context, server core.Server, _ core.LeaseClaim) error {
		client, err := b.clientFactory(b.Cfg, b.RT)
		if err != nil {
			return err
		}
		live, err := client.GetInstance(ctx, server.CloudID)
		if err != nil {
			return err
		}
		if err := validateLiveInstance(live, req.Lease.Server); err != nil {
			return err
		}
		return client.ReplaceInstanceTags(ctx, server.CloudID, live.Tags, tagsFromLabels(server.Labels))
	})
}
