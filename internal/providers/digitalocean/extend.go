package digitalocean

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *digitalOceanLeaseBackend) ExtendLease(ctx context.Context, req core.LeaseExtendRequest) (core.LeaseExtendResult, error) {
	return core.ExtendDirectLease(ctx, b, b.Cfg, req, core.ClockNow(b.RT.Clock).UTC(), func(ctx context.Context, server core.Server, claim core.LeaseClaim) error {
		client, err := b.clientFactory(b.RT)
		if err != nil {
			return err
		}
		live, err := client.GetDroplet(ctx, server.ID)
		if err != nil {
			return err
		}
		if err := validateLiveDroplet(live, req.Lease.Server); err != nil {
			return err
		}
		return client.ReplaceDropletTags(ctx, server.ID, live.Tags, tagsFromLabels(server.Labels))
	})
}
