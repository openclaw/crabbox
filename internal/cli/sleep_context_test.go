package cli

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

func TestSleepContextCompletesWhenContextStaysLive(t *testing.T) {
	t.Parallel()
	start := time.Now()
	err := sleepContext(context.Background(), 20*time.Millisecond)
	if err != nil {
		t.Fatalf("sleepContext returned %v, want nil", err)
	}
	if time.Since(start) < 15*time.Millisecond {
		t.Fatalf("sleepContext returned too early: %v", time.Since(start))
	}
}

func TestWaitForLoopbackVNCHonorsCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := waitForLoopbackVNC(ctx, &SSHTarget{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForLoopbackVNC returned %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("waitForLoopbackVNC took %v; expected immediate return on cancel", time.Since(start))
	}
}

func TestResolveVNCEndpointStaticHonorsCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := resolveVNCEndpoint(ctx, Config{Provider: staticProvider}, &SSHTarget{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("resolveVNCEndpoint returned %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("resolveVNCEndpoint took %v; expected immediate return on cancel", time.Since(start))
	}
}
