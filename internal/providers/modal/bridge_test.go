package modal

import (
	"context"
	"errors"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestModalBridgeIsExplicitlyUnsupported(t *testing.T) {
	backend := &modalBackend{}
	if _, err := backend.PublishPeer(context.Background(), "cbx_x", 8080, time.Hour); !errors.Is(err, core.ErrBridgeNotImplemented) {
		t.Fatalf("expected PublishPeer to return ErrBridgeNotImplemented, got %v", err)
	}
	if _, err := backend.ListPeerTargets(context.Background(), "cbx_x"); !errors.Is(err, core.ErrBridgeNotImplemented) {
		t.Fatalf("expected ListPeerTargets to return ErrBridgeNotImplemented, got %v", err)
	}
}

// Static check: ensure modalBackend satisfies core.BridgeProvider so the
// "unsupported" signal flows through the resolver instead of being silently
// dropped by the framework's fallback path.
var _ core.BridgeProvider = (*modalBackend)(nil)
