package vultr

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) ExtendLease(ctx context.Context, req core.LeaseExtendRequest) (core.LeaseExtendResult, error) {
	return core.ExtendDirectLease(ctx, b, b.Cfg, req, core.ClockNow(b.RT.Clock).UTC(), func(ctx context.Context, server core.Server, claim core.LeaseClaim) error {
		client, err := b.clientFactory(b.RT)
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
		return client.UpdateInstanceTags(ctx, live.ID, tagsFromLabels(server.Labels))
	})
}
