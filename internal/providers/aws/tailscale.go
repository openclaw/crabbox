package aws

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func (b *awsLeaseBackend) UpdateTailscaleMetadata(ctx context.Context, lease core.LeaseTarget, meta core.TailscaleMetadata) (core.Server, error) {
	return shared.PersistTailscaleMetadata(ctx, lease, meta, func(claim core.LeaseClaim, apply func(*core.Server)) (core.Server, error) {
		cfg := awsConfigForServer(b.Cfg, lease.Server)
		client, err := newAWSClient(ctx, cfg)
		if err != nil {
			return core.Server{}, err
		}
		server, err := client.GetServer(ctx, lease.Server.CloudID)
		if err != nil {
			return core.Server{}, err
		}
		server = annotateAWSServerRegion(server, cfg.AWSRegion)
		current := lease
		current.Server = server
		if err := authorizeAWSTouch(ctx, client, current, claim); err != nil {
			return core.Server{}, err
		}
		apply(&server)
		return server, client.SetTags(ctx, server.CloudID, server.Labels)
	})
}
