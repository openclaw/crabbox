package scaleway

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
)

func (b *Backend) ExtendLease(ctx context.Context, req core.LeaseExtendRequest) (core.LeaseExtendResult, error) {
	return core.ExtendDirectLease(ctx, b, b.cfgForRun(), req, b.clockNow(), func(ctx context.Context, server core.Server, claim core.LeaseClaim) error {
		client, err := b.newClient(b.cfgForRun(), b.rt)
		if err != nil {
			return err
		}
		if err := b.AuthorizeStatusTouchClaim(ctx, req.Lease, claim); err != nil {
			return err
		}
		item, err := client.Instance().GetServer(&instance.GetServerRequest{Zone: scw.Zone(client.Zone()), ServerID: server.CloudID}, scw.WithContext(ctx))
		if err != nil {
			return err
		}
		if item.Server == nil {
			return core.Exit(4, "Scaleway server disappeared")
		}
		if err := b.validateLiveServer(b.serverFromScaleway(item.Server), req.Lease.Server); err != nil {
			return err
		}
		_, err = client.Instance().UpdateServer(&instance.UpdateServerRequest{Zone: scw.Zone(client.Zone()), ServerID: server.CloudID, Tags: ptrTags(shared.ReplaceCrabboxTags(item.Server.Tags, tagsFromLabels(server.Labels)))}, scw.WithContext(ctx))
		return err
	})
}
