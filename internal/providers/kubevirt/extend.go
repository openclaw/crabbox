package kubevirt

import (
	"context"
	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *leaseBackend) ExtendLease(ctx context.Context, req core.LeaseExtendRequest) (core.LeaseExtendResult, error) {
	return core.ExtendDirectLease(ctx, b, b.cfg, req, core.ClockNow(b.rt.Clock).UTC(), func(ctx context.Context, server core.Server, _ core.LeaseClaim) error {
		if _, _, _, _, err := b.validateVMIdentity(ctx, server.Name, req.Lease.LeaseID, server.Labels["slug"]); err != nil {
			return err
		}
		return b.patchVMAnnotations(ctx, server.Name, server.Labels)
	})
}
