package opencomputer

import (
	"context"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// OpenComputer exposes preview URLs through its own `oc preview` surface and
// central control plane rather than the Crabbox bridge plane. As with modal,
// cloudflare, and tensorlake, the adapter implements core.BridgeProvider only
// to return core.ErrBridgeNotImplemented so the resolver flags the peer with
// BridgeState="unsupported" instead of silently emitting an empty Targets
// slice that callers could misread as "no shares yet".

// PublishPeer reports that OpenComputer does not participate in the bridge plane.
func (b *openComputerBackend) PublishPeer(ctx context.Context, leaseID string, port int, ttl time.Duration) (core.BridgePeerTarget, error) {
	_, _, _, _ = ctx, leaseID, port, ttl
	return core.BridgePeerTarget{}, core.ErrBridgeNotImplemented
}

// ListPeerTargets reports that OpenComputer does not participate in the bridge plane.
func (b *openComputerBackend) ListPeerTargets(ctx context.Context, leaseID string) ([]core.BridgePeerTarget, error) {
	_, _ = ctx, leaseID
	return nil, core.ErrBridgeNotImplemented
}
