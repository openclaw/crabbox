package cli

import (
	"context"
	"io"
	"testing"
	"time"
)

// TestWebVNCBridgeSlotBacksOffAcrossConnectFailures proves that
// serveWebVNCBridgeSlot loses the attempt counter across consecutive connect
// failures. At webvnc.go:470 the statement
//
//	attempt, kind := nextWebVNCBridgeFailure(connectedOnce, attempt)
//
// uses := inside the `if err != nil` block, declaring a NEW inner `attempt`
// that shadows the loop's counter (declared at webvnc.go:466). The outer
// counter therefore stays 0 forever, so every failed connect reports
// Attempt=1 / Kind="initial-error" and the reconnect delay is pinned at
// webVNCReconnectDelay(1)=500ms instead of backing off linearly to 5s.
//
// The intended contract is pinned by the existing unit test
// TestNextWebVNCBridgeFailureBacksOffInitialFailures (webvnc_test.go): when
// the returned attempt is threaded back in, the second failure must yield
// attempt=2 / kind="retry" (1s delay). This test exercises the real loop and
// asserts that same contract holds end-to-end.
//
// connectWebVNCBridge is made to fail instantly and deterministically (no
// sockets) by setting CRABBOX_WEBVNC_AGENT_BASE_URL to a value with leading
// whitespace, which webVNCAgentBaseURL rejects before any dial.
func TestWebVNCBridgeSlotBacksOffAcrossConnectFailures(t *testing.T) {
	t.Setenv("CRABBOX_WEBVNC_AGENT_BASE_URL", " https://invalid.example:443")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := webVNCBridgePoolConfig{
		Coord:   &CoordinatorClient{BaseURL: "https://coordinator.invalid"},
		LeaseID: "lease-",
		Host:    "127.0.0.1",
		Port:    "1", // never dialed: connect fails at webVNCAgentBaseURL
		Log:     io.Discard,
	}

	events := make(chan webVNCBridgePoolEvent, 16)
	go serveWebVNCBridgeSlot(ctx, cfg, 0, events)

	// Collect the first three failure events from the real slot loop.
	var got []webVNCBridgePoolEvent
	deadline := time.After(15 * time.Second)
	for len(got) < 3 {
		select {
		case ev := <-events:
			if ev.Kind == "fatal" {
				t.Fatalf("unexpected fatal event: %v", ev.Err)
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out waiting for failure events; got %d so far: %+v", len(got), got)
		}
	}
	cancel()

	wantAttempts := []int{1, 2, 3}
	wantKinds := []string{"initial-error", "retry", "retry"}
	for i, ev := range got {
		if ev.Attempt != wantAttempts[i] {
			t.Errorf("failure %d: Attempt=%d, want %d (attempt counter lost: shadowed by := at webvnc.go:470, backoff pinned at %s instead of %s)",
				i+1, ev.Attempt, wantAttempts[i],
				webVNCReconnectDelay(ev.Attempt), webVNCReconnectDelay(wantAttempts[i]))
		}
		if ev.Kind != wantKinds[i] {
			t.Errorf("failure %d: Kind=%q, want %q", i+1, ev.Kind, wantKinds[i])
		}
	}
}
