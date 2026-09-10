package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoordinatorOperationBudgets(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Provider = "aws"
	for _, test := range []struct {
		name string
		want time.Duration
		call func(context.Context, *CoordinatorClient) error
	}{
		{"lease", 30 * time.Second, func(ctx context.Context, c *CoordinatorClient) error {
			_, err := c.GetLease(ctx, "cbx_budget")
			return err
		}},
		{"authoritative lease", 30 * time.Second, func(ctx context.Context, c *CoordinatorClient) error {
			_, err := c.GetLeaseWithAuthoritativeProviderMetadata(ctx, "cbx_budget")
			return err
		}},
		{"health", 30 * time.Second, func(ctx context.Context, c *CoordinatorClient) error { return c.Health(ctx) }},
		{"identity", 30 * time.Second, func(ctx context.Context, c *CoordinatorClient) error { _, err := c.Whoami(ctx); return err }},
		{"readiness", 30 * time.Second, func(ctx context.Context, c *CoordinatorClient) error {
			_, err := c.ProviderReadiness(ctx, cfg)
			return err
		}},
		{"heartbeat", 30 * time.Second, func(ctx context.Context, c *CoordinatorClient) error {
			_, err := c.TouchLeaseForProvider(ctx, "cbx_budget", "aws")
			return err
		}},
		{"idle timeout", 30 * time.Second, func(ctx context.Context, c *CoordinatorClient) error {
			_, err := c.UpdateLeaseIdleTimeoutForProvider(ctx, "cbx_budget", "aws", time.Minute)
			return err
		}},
		{"create", 30 * time.Minute, func(ctx context.Context, c *CoordinatorClient) error {
			_, err := c.CreateLease(ctx, cfg, "synthetic-public-key", false, "cbx_budget", "")
			return err
		}},
		{"fixed create", 30 * time.Minute, func(ctx context.Context, c *CoordinatorClient) error {
			_, err := c.EnsureLease(ctx, cfg, "synthetic-public-key", false, "cbx_budget", "")
			return err
		}},
		{"image", 30 * time.Minute, func(ctx context.Context, c *CoordinatorClient) error {
			_, err := c.CreateImage(ctx, "cbx_budget", "budget-image", true)
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, _, err := newCoordinatorClient(Config{Coordinator: "http://127.0.0.1"})
			if err != nil {
				t.Fatal(err)
			}
			client.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				deadline, ok := req.Context().Deadline()
				remaining := time.Until(deadline)
				if !ok || remaining > test.want || remaining < test.want-5*time.Second {
					t.Errorf("%s %s budget=%s, want %s", req.Method, req.URL.Path, remaining, test.want)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
			})
			if err := test.call(context.Background(), client); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCoordinatorLeaseReadStallsAreBounded(t *testing.T) {
	// Explicit clients and private servers need no process-wide config changes.
	// Keep the real control deadlines without serializing their wall-clock waits.
	t.Parallel()
	for _, bodyStall := range []bool{false, true} {
		name := "headers"
		if bodyStall {
			name = "authoritative body"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if bodyStall {
					_, _ = io.WriteString(w, `{"lease":`)
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer func() { close(release); server.Close() }()
			client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
			ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
			defer cancel()
			var err error
			if bodyStall {
				_, err = client.GetLeaseWithAuthoritativeProviderMetadata(ctx, "cbx_budget")
			} else {
				_, err = client.GetLease(ctx, "cbx_budget")
			}
			if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || calls.Load() != 1 {
				t.Fatalf("read err=%v parent=%v requests=%d; want own deadline and no retry", err, ctx.Err(), calls.Load())
			}
		})
	}
}

func TestCoordinatorHeartbeatHonorsCallerDeadlineWithoutReplay(t *testing.T) {
	clearConfigEnv(t)
	// Keep this HTTP deadline test independent of local git process startup.
	t.Setenv("CRABBOX_OWNER", "alice@example.com")
	var calls atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() { close(release); server.Close() }()
	client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := client.TouchLeaseForProvider(ctx, "cbx_budget", "aws")
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("heartbeat err=%v requests=%d; want caller deadline and no replay", err, calls.Load())
	}
}

func TestCoordinatorReadCurlFallbackSharesCallerDeadline(t *testing.T) {
	clearConfigEnv(t)
	// Keep the HTTP deadline independent of local git process startup.
	t.Setenv("CRABBOX_OWNER", "alice@example.com")
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is unavailable")
	}
	t.Setenv("NO_PROXY", "127.0.0.1")
	t.Setenv("no_proxy", "127.0.0.1")
	var calls atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"lease":`)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() { close(release); server.Close() }()
	client := &CoordinatorClient{BaseURL: server.URL, Client: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if err := sleepContext(req.Context(), 100*time.Millisecond); err != nil {
				return nil, err
			}
			return nil, io.ErrUnexpectedEOF
		}),
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	started := time.Now()
	_, err := client.GetLease(ctx, "cbx_budget")
	if err == nil || !strings.Contains(err.Error(), "curl fallback failed") || ctx.Err() != context.DeadlineExceeded || calls.Load() != 1 || time.Since(started) > 5*time.Second {
		t.Fatalf("fallback err=%v parent=%v requests=%d elapsed=%s", err, ctx.Err(), calls.Load(), time.Since(started))
	}
}

func TestCoordinatorReadCurlFallbackAfterDialTimeout(t *testing.T) {
	clearConfigEnv(t)
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is unavailable")
	}
	t.Setenv("CRABBOX_OWNER", "alice@example.com")
	t.Setenv("NO_PROXY", "127.0.0.1")
	t.Setenv("no_proxy", "127.0.0.1")
	var nativeCalls, curlCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		curlCalls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/leases/cbx_dial" ||
			r.Header.Get("Authorization") != "Bearer synthetic-dial-token" {
			t.Errorf("unexpected fallback request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"lease":{"id":"cbx_dial","provider":"aws","state":"released"}}`)
	}))
	defer server.Close()
	client := &CoordinatorClient{BaseURL: server.URL, Token: "synthetic-dial-token", Client: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			nativeCalls.Add(1)
			// Expire only the dialer's own budget, not the logical request.
			dialer := net.Dialer{Deadline: time.Now().Add(-time.Second)}
			conn, err := dialer.DialContext(req.Context(), "tcp", server.Listener.Addr().String())
			if conn != nil {
				_ = conn.Close()
				t.Fatal("expired dial unexpectedly connected")
			}
			var dialErr *net.OpError
			if !errors.As(err, &dialErr) || dialErr.Op != "dial" || !dialErr.Timeout() ||
				!errors.Is(err, context.DeadlineExceeded) || req.Context().Err() != nil {
				t.Fatalf("dial error=%v request=%v, want dial-local deadline with live request", err, req.Context().Err())
			}
			return nil, err
		}),
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	lease, err := client.GetLease(ctx, "cbx_dial")
	if err != nil || lease.ID != "cbx_dial" || lease.Provider != "aws" || lease.State != "released" ||
		nativeCalls.Load() != 1 || curlCalls.Load() != 1 || ctx.Err() != nil {
		t.Fatalf("live-parent dial fallback: err=%v lease=%s/%s/%s native=%d curl=%d parent=%v",
			err, lease.ID, lease.Provider, lease.State, nativeCalls.Load(), curlCalls.Load(), ctx.Err())
	}
}

func TestCoordinatorReadCurlFallbackRejectsIneligibleRequests(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_OWNER", "alice@example.com")
	t.Setenv("NO_PROXY", "127.0.0.1")
	t.Setenv("no_proxy", "127.0.0.1")
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}
	for _, test := range []struct {
		name         string
		method       string
		body         any
		err          error
		cancelDuring bool
		expired      bool
	}{
		{"canceled during native attempt", http.MethodGet, nil, io.ErrUnexpectedEOF, true, false},
		{"expired parent", http.MethodGet, nil, dialErr, false, true},
		{"GET with body", http.MethodGet, map[string]string{"query": "fixture"}, dialErr, false, false},
		{"HEAD with body", http.MethodHead, map[string]string{"query": "fixture"}, dialErr, false, false},
		{"POST", http.MethodPost, nil, dialErr, false, false},
		{"PUT", http.MethodPut, nil, dialErr, false, false},
		{"PATCH", http.MethodPatch, nil, dialErr, false, false},
		{"DELETE", http.MethodDelete, nil, dialErr, false, false},
		{"CONNECT", http.MethodConnect, nil, dialErr, false, false},
		{"OPTIONS", http.MethodOptions, nil, dialErr, false, false},
		{"TRACE", http.MethodTrace, nil, dialErr, false, false},
		{"request deadline", http.MethodGet, nil, context.DeadlineExceeded, false, false},
		{"read deadline", http.MethodGet, nil, &net.OpError{Op: "read", Net: "tcp", Err: context.DeadlineExceeded}, false, false},
		{"dial cancellation", http.MethodGet, nil, &net.OpError{Op: "dial", Net: "tcp", Err: context.Canceled}, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var nativeCalls, curlCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				curlCalls.Add(1)
				_, _ = io.WriteString(w, `{}`)
			}))
			defer server.Close()
			deadline := time.Now().Add(5 * time.Second)
			if test.expired {
				deadline = time.Now().Add(-time.Second)
			}
			ctx, cancel := context.WithDeadline(t.Context(), deadline)
			defer cancel()
			client := &CoordinatorClient{BaseURL: server.URL, Client: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					nativeCalls.Add(1)
					if req.Context().Err() != nil {
						t.Fatal("native attempt started with an expired context")
					}
					if test.cancelDuring {
						cancel()
					}
					return nil, test.err
				}),
			}}
			err := client.do(ctx, test.method, "/v1/health", test.body, nil)
			wantCalls := int32(1)
			wantErr := test.err
			if test.expired {
				wantCalls, wantErr = 0, context.DeadlineExceeded
			}
			if err == nil || !errors.Is(err, wantErr) || strings.Contains(err.Error(), "curl fallback failed") ||
				nativeCalls.Load() != wantCalls || curlCalls.Load() != 0 {
				t.Fatalf("err=%v native=%d curl=%d, want original error, %d native calls and no fallback",
					err, nativeCalls.Load(), curlCalls.Load(), wantCalls)
			}
		})
	}
}

func TestStopCoordinatorStalledLookup(t *testing.T) {
	if runParallelCLIContract(t, 0) {
		return
	}
	for _, mode := range []string{"release", "force", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			ctx, cancelCause := context.WithCancelCause(ctx)
			defer cancelCause(nil)
			cause := errors.New("stop caller canceled")
			var gets, releases atomic.Int32
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/cbx_abcdef123456":
					gets.Add(1)
					if mode == "canceled" {
						cancelCause(cause)
					}
					select {
					case <-r.Context().Done():
					case <-release:
					}
				case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/cbx_abcdef123456/release":
					releases.Add(1)
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["delete"] != true || body["expectedProvider"] != "aws" || r.Header.Get("Authorization") != "Bearer synthetic-stop-token" {
						t.Errorf("invalid scoped release: body=%v decode=%v", body, err)
						http.Error(w, "invalid release", http.StatusBadRequest)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"lease": confirmedCoordinatorRelease("cbx_abcdef123456", "aws")})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer func() { close(release); server.Close() }()
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "synthetic-stop-token")
			args := []string{"--provider", "aws", "--id", "cbx_abcdef123456"}
			if mode == "force" {
				args = append(args, "--force")
			}
			var stderr bytes.Buffer
			err := (App{Stdout: io.Discard, Stderr: &stderr}).stop(ctx, args)
			if gets.Load() != 1 {
				t.Fatalf("GETs=%d, want one", gets.Load())
			}
			switch mode {
			case "release":
				if err != nil || releases.Load() != 1 || ctx.Err() != nil || !strings.Contains(stderr.String(), "could not inspect lease before release") {
					t.Fatalf("stop err=%v releases=%d parent=%v stderr=%s", err, releases.Load(), ctx.Err(), stderr.String())
				}
			case "force":
				if !errors.Is(err, context.DeadlineExceeded) || releases.Load() != 0 || ctx.Err() != nil {
					t.Fatalf("forced stop err=%v releases=%d parent=%v", err, releases.Load(), ctx.Err())
				}
			case "canceled":
				if !errors.Is(err, cause) || releases.Load() != 0 {
					t.Fatalf("canceled stop err=%v releases=%d", err, releases.Load())
				}
			}
		})
	}
}
