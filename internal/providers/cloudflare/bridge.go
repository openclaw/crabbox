package cloudflare

import (
	"context"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Cloudflare Container sandboxes are reached through a single Worker URL
// gated by a bearer token. There is no per-sandbox HTTPS preview that an
// unrelated peer could dial without that token — the runner is the only
// authorised caller. The bridge plane is HTTP-only and intentionally
// requires public per-sandbox ingress, so cloudflare is explicitly
// unsupported rather than silently broken.
//
// The adapter still satisfies core.BridgeProvider so explicit bridge calls fail
// with the same honest "unsupported" signal if this provider is ever routed
// through the bridge path.

// PublishPeer reports that Cloudflare does not participate in the bridge plane.
func (b *cloudflareBackend) PublishPeer(ctx context.Context, leaseID string, port int, ttl time.Duration) (core.BridgePeerTarget, error) {
	_, _, _, _ = ctx, leaseID, port, ttl
	return core.BridgePeerTarget{}, core.ErrBridgeNotImplemented
}

// ListPeerTargets reports that Cloudflare does not participate in the bridge plane.
func (b *cloudflareBackend) ListPeerTargets(ctx context.Context, leaseID string) ([]core.BridgePeerTarget, error) {
	_, _ = ctx, leaseID
	return nil, core.ErrBridgeNotImplemented
}
