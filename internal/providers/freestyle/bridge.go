package freestyle

import (
	"context"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *freestyleBackend) PublishPeer(ctx context.Context, leaseID string, port int, ttl time.Duration) (core.BridgePeerTarget, error) {
	return core.BridgePeerTarget{}, core.ErrBridgeNotImplemented
}

func (b *freestyleBackend) ListPeerTargets(ctx context.Context, leaseID string) ([]core.BridgePeerTarget, error) {
	return nil, core.ErrBridgeNotImplemented
}
