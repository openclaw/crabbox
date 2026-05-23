package freestyle

import (
	"context"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestPublishPeerReturnsErrBridgeNotImplemented(t *testing.T) {
	backend := &freestyleBackend{
		cfg: Config{Freestyle: FreestyleConfig{APIKey: "key"}},
	}
	_, err := backend.PublishPeer(context.Background(), "fsb_crabbox-x-abc123", 8080, 0)
	if err != core.ErrBridgeNotImplemented {
		t.Fatalf("expected ErrBridgeNotImplemented, got %v", err)
	}
}

func TestListPeerTargetsReturnsErrBridgeNotImplemented(t *testing.T) {
	backend := &freestyleBackend{
		cfg: Config{Freestyle: FreestyleConfig{APIKey: "key"}},
	}
	_, err := backend.ListPeerTargets(context.Background(), "fsb_crabbox-x-abc123")
	if err != core.ErrBridgeNotImplemented {
		t.Fatalf("expected ErrBridgeNotImplemented, got %v", err)
	}
}

var _ core.BridgeProvider = (*freestyleBackend)(nil)
