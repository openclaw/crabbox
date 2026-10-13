package sealosdevbox

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) ExtendLease(ctx context.Context, req core.LeaseExtendRequest) (core.LeaseExtendResult, error) {
	return core.ExtendDirectLease(ctx, b, b.cfg, req, b.now(), func(ctx context.Context, server core.Server, claim core.LeaseClaim) error {
		item, name, id, slug, err := b.resolveDevbox(ctx, req.Lease.LeaseID)
		if err != nil {
			return err
		}
		if id != req.Lease.LeaseID || slug != server.Labels["slug"] {
			return core.Exit(4, "Sealos DevBox lease identity changed")
		}
		if err := b.validateClaimBinding(claim, item); err != nil {
			return err
		}
		return b.patchDevboxAnnotations(ctx, name, item.Metadata.ResourceVersion, annotationsFromLeaseLabels(server.Labels))
	})
}
