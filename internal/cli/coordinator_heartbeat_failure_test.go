package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

type heartbeatWarningWriter struct{ lines chan string }

func (w heartbeatWarningWriter) Write(p []byte) (int, error) {
	select {
	case w.lines <- string(p):
	default:
	}
	return len(p), nil
}

func TestCoordinatorHeartbeatControlFailure(t *testing.T) {
	// Explicit clients and private servers let the real deadlines overlap.
	t.Parallel()
	for _, mode := range []string{"expired budget", "immediate rejection", "failed fallback", "caller cancellation"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var httpRequests atomic.Int32
			received := make(chan struct{}, 1)
			fallback := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer synthetic-fixture-token" {
					t.Error("heartbeat did not use the explicit fixture token")
				}
				switch r.URL.Path {
				case "/v1/control":
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.CloseNow()
					if _, _, err = conn.Read(r.Context()); err != nil {
						t.Error(err)
						return
					}
					received <- struct{}{}
					if mode == "immediate rejection" || mode == "failed fallback" {
						_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"heartbeat","ok":false,"error":"synthetic_rejection"}`))
					}
					// Keep the real socket open until the client's deadline or cancellation.
					_, _, _ = conn.Read(r.Context())
				case "/v1/leases/cbx_synthetic/heartbeat":
					httpRequests.Add(1)
					if mode == "failed fallback" {
						http.Error(w, "synthetic HTTP failure", http.StatusServiceUnavailable)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"lease":{"id":"cbx_synthetic","provider":"aws","state":"active"}}`)
					fallback <- struct{}{}
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			warnings := make(chan string, 4)
			client := CoordinatorClient{BaseURL: server.URL, Token: "synthetic-fixture-token", Client: server.Client()}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			started := time.Now()
			stop, err := startCoordinatorHeartbeat(ctx, &client, "cbx_synthetic", "aws", 30*time.Minute, nil, nil, heartbeatWarningWriter{warnings})
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			select {
			case <-received:
			case <-time.After(3 * time.Second):
				t.Fatal("real WebSocket never received heartbeat")
			}
			switch mode {
			case "expired budget":
				select {
				case warning := <-warnings:
					t.Logf("elapsed=%s httpHeartbeatRequests=%d warning=%q", time.Since(started), httpRequests.Load(), warning)
					if !strings.Contains(warning, "control heartbeat") {
						t.Errorf("lost failing transport diagnosis: %q", warning)
					}
				case <-time.After(27 * time.Second):
					t.Fatal("heartbeat did not surface its bounded failure")
				}
				if httpRequests.Load() != 0 {
					t.Error("expired context dispatched an HTTP mutation")
				}
			case "immediate rejection":
				select {
				case <-fallback:
				case <-time.After(3 * time.Second):
					t.Fatal("live heartbeat budget did not fall back to HTTP")
				}
				select {
				case warning := <-warnings:
					t.Errorf("successful HTTP fallback reported a background failure: %q", warning)
				case <-time.After(100 * time.Millisecond):
				}
				stop()
				if got := httpRequests.Load(); got != 1 {
					t.Errorf("HTTP heartbeat requests=%d, want one successful fallback", got)
				}
				t.Logf("httpHeartbeatRequests=%d; successful fallback stayed quiet", httpRequests.Load())
			case "failed fallback":
				select {
				case warning := <-warnings:
					t.Logf("httpHeartbeatRequests=%d warning=%q", httpRequests.Load(), warning)
					if !strings.Contains(warning, "synthetic_rejection") || !strings.Contains(warning, "http 503") {
						t.Errorf("lost one transport failure: %q", warning)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("failed HTTP fallback was not reported")
				}
			case "caller cancellation":
				cancel()
				stop()
				if httpRequests.Load() != 0 {
					t.Error("caller cancellation dispatched an HTTP mutation")
				}
				select {
				case warning := <-warnings:
					t.Errorf("caller cancellation reported a background failure: %q", warning)
				default:
				}
				t.Logf("httpHeartbeatRequests=%d; canceled heartbeat owner joined without warning", httpRequests.Load())
			}
		})
	}
}
