package shared

import (
	"context"
	"fmt"

	core "github.com/openclaw/crabbox/internal/cli"
)

// PersistTailscaleMetadata fences the provider write and endpoint claim together.
// The adapter must revalidate its native resource identity in update before writing.
func PersistTailscaleMetadata(ctx context.Context, lease core.LeaseTarget, meta core.TailscaleMetadata, update func(core.LeaseClaim, func(*core.Server)) (core.Server, error)) (core.Server, error) {
	expected, exists, set := core.ServerLeaseClaimSnapshot(lease.Server)
	if !set || !exists || expected.LeaseID != lease.LeaseID {
		return core.Server{}, fmt.Errorf("tailscale metadata update requires an exact lease claim snapshot")
	}
	claim, server, _, err := core.UpdateLeaseClaimEndpointIfUnchangedAction(lease.LeaseID, expected, func() (core.Server, core.SSHTarget, bool, error) {
		if err := ctx.Err(); err != nil {
			return core.Server{}, core.SSHTarget{}, false, err
		}
		server, err := update(expected, func(server *core.Server) {
			server.Labels = CloneLabels(server.Labels)
			ApplyTailscaleMetadata(server.Labels, meta)
		})
		return server, lease.SSH, err == nil, err
	})
	if err != nil {
		return core.Server{}, err
	}
	core.SetServerLeaseClaimSnapshot(&server, claim, true)
	return server, nil
}
