package linode

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func (b *linodeLeaseBackend) ExtendLease(ctx context.Context, req core.LeaseExtendRequest) (core.LeaseExtendResult, error) {
	return core.ExtendDirectLease(ctx, b, b.Cfg, req, core.ClockNow(b.RT.Clock).UTC(), func(ctx context.Context, server core.Server, claim core.LeaseClaim) error {
		client, err := b.clientFactory(b.RT)
		if err != nil {
			return err
		}
		account, err := client.AccountID(ctx)
		if err != nil {
			return err
		}
		if account != claim.Labels[linodeAccountLabel] {
			return core.Exit(4, "Linode account changed; refusing extension")
		}
		if err := validateCleanupClaim(req.Lease.Server, claim, false); err != nil {
			return err
		}
		live, err := client.GetLinode(ctx, server.ID)
		if err != nil {
			return err
		}
		if err := validateLiveLinode(live, req.Lease.Server); err != nil {
			return err
		}
		return client.UpdateLinodeTags(ctx, server.ID, shared.ReplaceCrabboxTags(live.Tags, tagsFromLabels(server.Labels)))
	})
}
