package localcontainer

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestDirectBackendEmptyImageUsesReviewedDefault(t *testing.T) {
	cfg := core.Config{}
	applyDefaults(&cfg)
	if digest, known := core.DefaultContainerImageDigest(cfg.LocalContainer.Image); !known || digest == "" {
		t.Fatalf("direct backend fallback is not a reviewed immutable image: %q", cfg.LocalContainer.Image)
	}
	cfg.LocalContainer.Image = "example-org/custom:latest"
	applyDefaults(&cfg)
	if cfg.LocalContainer.Image != "example-org/custom:latest" {
		t.Fatal("direct backend replaced explicit custom image")
	}
}
