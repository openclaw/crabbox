package aws

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *awsLeaseBackend) ExtendLease(ctx context.Context, req core.LeaseExtendRequest) (core.LeaseExtendResult, error) {
	return core.ExtendDirectLease(ctx, b, b.Cfg, req, core.ClockNow(b.RT.Clock).UTC(), func(ctx context.Context, server core.Server, claim core.LeaseClaim) error {
		client, err := newAWSClient(ctx, awsConfigForServer(b.Cfg, server))
		if err != nil {
			return err
		}
		live, err := client.GetServer(ctx, server.CloudID)
		if err != nil {
			return err
		}
		live = annotateAWSServerRegion(live, awsConfigForServer(b.Cfg, server).AWSRegion)
		lease := req.Lease
		lease.Server = live
		if err := authorizeAWSTouch(ctx, client, lease, claim); err != nil {
			return err
		}
		return client.SetTags(ctx, server.CloudID, server.Labels)
	})
}
