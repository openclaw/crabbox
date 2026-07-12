package digitalocean

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSleepContextHonorsCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := sleepContext(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepContext returned %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("sleepContext took %v; expected immediate return on cancel", time.Since(start))
	}
}
