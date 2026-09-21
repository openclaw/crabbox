package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSSHManagedLeasePreservesCoordinatorIdlePolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses the POSIX recording SSH fixture")
	}
	for _, tc := range []struct {
		name         string
		recordedIdle int
	}{
		{name: "existing policy", recordedIdle: 10800},
		{name: "stale policy", recordedIdle: 1800},
		{name: "fresh claim"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease := CoordinatorLease{
				ID: "cbx_abcdef123456", Slug: "managed-idle", Provider: "aws", TargetOS: targetLinux,
				CloudID: "i-managed-idle", Host: "127.0.0.1", SSHUser: "runner", SSHPort: "2222",
				State: "active", IdleTimeoutSeconds: 10800,
			}
			requests := make(chan map[string]any, 1)
			coordinator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer user-token" {
					t.Error("coordinator request did not use the fixture token")
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+lease.ID:
				case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/"+lease.ID+"/heartbeat":
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						http.Error(w, "invalid heartbeat", http.StatusBadRequest)
						return
					}
					select {
					case requests <- body:
					default:
						t.Error("SSH sent more than one heartbeat")
					}
				default:
					t.Errorf("unexpected coordinator request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
			}))
			defer coordinator.Close()
			configureHeartbeatCoordinatorTest(t, coordinator.URL)
			t.Setenv("CRABBOX_IDLE_TIMEOUT", "")
			t.Setenv("CRABBOX_OWNER", "alice@example.test")
			t.Setenv("CRABBOX_ADAPTER_ID", "")
			t.Setenv(controllerWorkspaceIDEnv, "")
			dir := t.TempDir()
			t.Chdir(dir)
			installRecordingSSH(t, dir)
			key := filepath.Join(dir, "fixture-key")
			if err := os.WriteFile(key, []byte("synthetic SSH key\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_SSH_KEY", key)
			cfg, err := loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.IdleTimeout != 30*time.Minute {
				t.Fatalf("fixture idle timeout=%s, want the 30-minute CLI default", cfg.IdleTimeout)
			}
			setProviderSelection(&cfg, lease.Provider, providerSelectionFlag)
			if tc.recordedIdle > 0 {
				seed := lease
				seed.IdleTimeoutSeconds = tc.recordedIdle
				server, target, _ := leaseToServerTarget(seed, cfg)
				repo, err := findRepo()
				if err != nil {
					t.Fatal(err)
				}
				if err := ClaimLeaseTargetForRepoConfig(lease.ID, lease.Slug, cfg, server, target, repo.Root, time.Duration(tc.recordedIdle)*time.Second, false); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			if err := (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"ssh", "--provider", "aws", "--network", "public", lease.ID}); err != nil {
				t.Fatalf("ssh error=%v stderr=%q", err, stderr.String())
			}
			if !strings.Contains(stdout.String(), "runner@127.0.0.1") || stderr.Len() != 0 {
				t.Fatalf("ssh stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			select {
			case body := <-requests:
				if _, overridden := body["idleTimeoutSeconds"]; overridden || body["expectedProvider"] != "aws" {
					t.Fatalf("ordinary SSH changed broker idle policy: heartbeat=%v", body)
				}
			default:
				t.Fatal("SSH did not send its lease heartbeat")
			}
			claim, err := ReadLeaseClaim(lease.ID)
			if err != nil {
				t.Fatal(err)
			}
			if claim.IdleTimeoutSeconds != 10800 || claim.Labels["idle_timeout_secs"] != "10800" {
				t.Fatalf("SSH replaced coordinator idle policy with a local default: idle=%d label=%q", claim.IdleTimeoutSeconds, claim.Labels["idle_timeout_secs"])
			}
		})
	}
}

func TestBestEffortLeaseTouchHTTPBudget(t *testing.T) {
	// Each case owns its config and HTTP server, not process-wide environment.
	t.Parallel()
	for _, mode := range []string{"success", "http error", "maintenance timeout", "parent cancellation", "parent deadline"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			if mode == "parent deadline" {
				var shorterCancel context.CancelFunc
				ctx, shorterCancel = context.WithTimeout(ctx, 3*time.Second)
				defer shorterCancel()
			}
			cause := errors.New("caller canceled lease maintenance")
			ctx, cancelCause := context.WithCancelCause(ctx)
			defer cancelCause(nil)
			release := make(chan struct{})
			finished := make(chan struct{})
			requests := make(chan map[string]any, 1)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) != 1 {
					http.Error(w, "unexpected extra request", http.StatusBadRequest)
					return
				}
				defer close(finished)
				var body map[string]any
				decodeErr := json.NewDecoder(r.Body).Decode(&body)
				_, _ = io.Copy(io.Discard, r.Body)
				_ = r.Body.Close()
				if decodeErr != nil || r.Method != http.MethodPost || r.URL.Path != "/v1/leases/cbx_touch_budget/heartbeat" || r.Header.Get("Authorization") != "Bearer synthetic-touch-token" {
					body = map[string]any{"invalidRequest": true}
				}
				requests <- body
				switch mode {
				case "success":
					_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
						ID: "cbx_touch_budget", Provider: "aws", State: "active", Slug: "refreshed",
					}})
				case "http error":
					http.Error(w, "maintenance unavailable", http.StatusServiceUnavailable)
				default:
					if mode == "parent cancellation" {
						cancelCause(cause)
					}
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}
			}))
			defer func() { close(release); server.Close() }()
			cfg := baseConfig()
			setProviderSelection(&cfg, "aws", providerSelectionFlag)
			cfg.Coordinator, cfg.CoordToken = server.URL, "synthetic-touch-token"
			lease := LeaseTarget{LeaseID: "cbx_touch_budget", Server: Server{
				Provider: "aws", CloudID: "original", Labels: map[string]string{"state": "ready"},
			}}
			var stderr bytes.Buffer
			got := (App{Stderr: &stderr}).touchLeaseTargetBestEffort(ctx, cfg, lease, "")
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("heartbeat handler did not exit after the touch returned")
			}
			if body := <-requests; !reflect.DeepEqual(body, map[string]any{"expectedProvider": "aws"}) || calls.Load() != 1 {
				t.Fatalf("heartbeat body=%v calls=%d", body, calls.Load())
			}
			if mode == "success" {
				if got.Labels["slug"] != "refreshed" || stderr.Len() != 0 {
					t.Fatalf("refresh=%#v stderr=%q", got, stderr.String())
				}
			} else if !reflect.DeepEqual(got, lease.Server) || strings.Count(stderr.String(), "warning: touch failed") != 1 {
				t.Fatalf("failed touch changed server or warning count: server=%#v stderr=%q", got, stderr.String())
			}
			switch mode {
			case "maintenance timeout":
				if ctx.Err() != nil || !strings.Contains(stderr.String(), "context deadline exceeded") {
					t.Fatalf("maintenance consumed parent budget: parent=%v stderr=%q", ctx.Err(), stderr.String())
				}
			case "parent cancellation":
				if !errors.Is(context.Cause(ctx), cause) {
					t.Fatalf("parent cause lost: %v", context.Cause(ctx))
				}
			case "parent deadline":
				if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
					t.Fatalf("shorter parent deadline lost: %v", ctx.Err())
				}
			}
		})
	}
}

func TestClaimAndTouchPreservesParentCancellation(t *testing.T) {
	for _, command := range []string{"claim", "ssh"} {
		for _, when := range []string{"before", "during"} {
			t.Run(command+"/"+when, func(t *testing.T) {
				lease, _ := setupRunClaimSnapshotTest(t)
				cfg := baseConfig()
				setProviderSelection(&cfg, runEnvProfileTestProvider{}.Spec().Name, providerSelectionFlag)
				cause := errors.New("caller canceled before transport")
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				calls := 0
				runEnvProfileTestTouchHook = func(TouchRequest) error {
					calls++
					cancel(cause)
					return context.Canceled
				}
				t.Cleanup(func() { runEnvProfileTestTouchHook = nil })
				if when == "before" {
					cancel(cause)
				}
				var stdout, stderr bytes.Buffer
				app := App{Stdout: &stdout, Stderr: &stderr}
				var err error
				if command == "ssh" {
					err = app.ssh(ctx, []string{"--provider", cfg.Provider, "--id", lease.LeaseID})
				} else {
					err = app.claimAndTouchLeaseTarget(ctx, cfg, &lease.Server, lease.SSH, lease.LeaseID, false)
				}
				wantCalls := 1
				if when == "before" {
					wantCalls = 0
				}
				if !errors.Is(err, cause) || stdout.Len() != 0 || calls != wantCalls {
					t.Fatalf("err=%v stdout=%q touches=%d want=%d stderr=%q", err, stdout.String(), calls, wantCalls, stderr.String())
				}
			})
		}
	}
}
