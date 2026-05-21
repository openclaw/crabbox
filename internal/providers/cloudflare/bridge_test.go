package cloudflare

import (
	"context"
	"errors"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCloudflareBridgeIsExplicitlyUnsupported(t *testing.T) {
	backend := &cloudflareBackend{}
	if _, err := backend.PublishPeer(context.Background(), "cbx_x", 8080, time.Hour); !errors.Is(err, core.ErrBridgeNotImplemented) {
		t.Fatalf("expected PublishPeer to return ErrBridgeNotImplemented, got %v", err)
	}
	if _, err := backend.ListPeerTargets(context.Background(), "cbx_x"); !errors.Is(err, core.ErrBridgeNotImplemented) {
		t.Fatalf("expected ListPeerTargets to return ErrBridgeNotImplemented, got %v", err)
	}
}

// Static check: ensure cloudflareBackend satisfies core.BridgeProvider.
var _ core.BridgeProvider = (*cloudflareBackend)(nil)
