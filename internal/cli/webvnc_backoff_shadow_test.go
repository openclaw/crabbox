package cli

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestWebVNCBridgeSlotBacksOffAcrossConnectFailures(t *testing.T) {
	// Leading whitespace fails split-agent URL validation before any network dial.
	t.Setenv("CRABBOX_WEBVNC_AGENT_BASE_URL", " https://invalid.example:443")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := webVNCBridgePoolConfig{
		Coord:   &CoordinatorClient{BaseURL: "https://coordinator.invalid"},
		LeaseID: "lease-",
		Host:    "127.0.0.1",
		Port:    "1",
		Log:     io.Discard,
	}

	events := make(chan webVNCBridgePoolEvent, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveWebVNCBridgeSlot(ctx, cfg, 0, events)
	}()

	var got []webVNCBridgePoolEvent
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for len(got) < 3 {
		select {
		case ev := <-events:
			if ev.Kind == "fatal" {
				t.Fatalf("unexpected fatal event: %v", ev.Err)
			}
			got = append(got, ev)
		case <-deadline.C:
			t.Fatalf("timed out waiting for failure events; got %d so far: %+v", len(got), got)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WebVNC bridge slot did not stop after cancellation")
	}

	wantAttempts := []int{1, 2, 3}
	wantKinds := []string{"initial-error", "retry", "retry"}
	for i, ev := range got {
		if ev.Attempt != wantAttempts[i] {
			t.Errorf("failure %d: attempt=%d delay=%s, want attempt=%d delay=%s",
				i+1, ev.Attempt, webVNCReconnectDelay(ev.Attempt),
				wantAttempts[i], webVNCReconnectDelay(wantAttempts[i]))
		}
		if ev.Kind != wantKinds[i] {
			t.Errorf("failure %d: Kind=%q, want %q", i+1, ev.Kind, wantKinds[i])
		}
	}
}
