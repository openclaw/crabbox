package modal

import (
	"context"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Modal sandboxes do not carry a per-sandbox HTTPS ingress URL on the lease
// record today. Modal does expose tunnels via `Sandbox.tunnels()` in the
// Python SDK, but the existing `modalSandbox` struct that the rest of this
// backend works with does not populate them — extending the provider to
// surface tunnels would be a runtime-behavior change, which is explicitly
// out of scope for PR #136.
//
// So the modal adapter implements core.BridgeProvider only to return
// core.ErrBridgeNotImplemented. The crew bridge resolver maps that error to
// BridgeState="unsupported" on the peer, which is honest reporting: callers
// see modal peers listed in the crew with a clear "no bridge plane for this
// provider" signal instead of a silently empty Targets slice.

// PublishPeer reports that Modal does not participate in the bridge plane.
func (b *modalBackend) PublishPeer(ctx context.Context, leaseID string, port int, ttl time.Duration) (core.BridgePeerTarget, error) {
	_, _, _, _ = ctx, leaseID, port, ttl
	return core.BridgePeerTarget{}, core.ErrBridgeNotImplemented
}

// ListPeerTargets reports that Modal does not participate in the bridge plane.
func (b *modalBackend) ListPeerTargets(ctx context.Context, leaseID string) ([]core.BridgePeerTarget, error) {
	_, _ = ctx, leaseID
	return nil, core.ErrBridgeNotImplemented
}
