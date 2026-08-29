package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	clearConfigEnv(t)
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

func TestStopCoordinatorStalledLookup(t *testing.T) {
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
					_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: "cbx_abcdef123456", Provider: "aws", State: "released"}})
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
