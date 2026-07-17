package digitalocean

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitForDropletIPHonorsCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	backend := &digitalOceanLeaseBackend{}
	api := &fakeDigitalOceanAPI{droplets: []droplet{{ID: 42}}}
	_, err := backend.waitForDropletIP(ctx, api, 42)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForDropletIP returned %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("waitForDropletIP took %v; expected immediate return on cancel", time.Since(start))
	}
}
