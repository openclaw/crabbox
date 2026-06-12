package tensorlake

import (
	"context"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Tensorlake is a serverless function platform — runs are scheduled as
// invocations of registered Tensorlake functions, not as long-lived sandboxes
// with their own HTTPS ingress. There is no per-sandbox URL on the lease
// record because there is no per-sandbox endpoint in the platform; ingress is
// owned by Tensorlake's central API.
//
// As with modal and cloudflare, the adapter implements core.BridgeProvider
// only to return core.ErrBridgeNotImplemented. If this provider is ever
// explicitly routed through the bridge path, callers get a clear unsupported
// signal instead of a silently empty Targets slice that could be misread as "no
// shares yet".

// PublishPeer reports that Tensorlake does not participate in the bridge plane.
func (b *tensorlakeBackend) PublishPeer(ctx context.Context, leaseID string, port int, ttl time.Duration) (core.BridgePeerTarget, error) {
	_, _, _, _ = ctx, leaseID, port, ttl
	return core.BridgePeerTarget{}, core.ErrBridgeNotImplemented
}

// ListPeerTargets reports that Tensorlake does not participate in the bridge plane.
func (b *tensorlakeBackend) ListPeerTargets(ctx context.Context, leaseID string) ([]core.BridgePeerTarget, error) {
	_, _ = ctx, leaseID
	return nil, core.ErrBridgeNotImplemented
}
