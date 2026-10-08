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
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoordinatorStopObservesExpiryCleanup(t *testing.T) {
	clearConfigEnv(t)
	configureCoordinatorReleaseTestTiming(t, time.Second, 0)
	const id = "cbx_abcdef123456"
	keyPath, claimPath := managedStopLocalState(t, id)
	claimBefore, err := os.ReadFile(claimPath)
	if err != nil {
		t.Fatal(err)
	}
	var posts, observations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lease := CoordinatorLease{
			ID: id, Provider: "aws", TargetOS: targetLinux, State: "active", CloudID: "i-original",
			CleanupStartedAt: "2026-10-08T00:00:00Z",
		}
		if r.URL.Path != "/v1/leases/"+id && r.URL.Path != "/v1/leases/"+id+"/release" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["delete"] != true || body["expectedProvider"] != "aws" {
				t.Errorf("release body=%v error=%v", body, err)
			}
			if posts.Add(1) > 1 {
				if observations.Load() < 2 {
					t.Error("repeated release before expiry cleanup completed")
				}
				lease = confirmedCoordinatorRelease(id, "aws")
				lease.CloudID = "i-original"
			}
		} else if r.Method == http.MethodGet && posts.Load() > 0 {
			if observations.Add(1) >= 2 {
				lease.State = "expired"
				lease.CleanupStartedAt = ""
				lease.CleanupCompletedAt = "2026-10-08T00:01:00Z"
			}
		}
		if after, err := os.ReadFile(claimPath); err != nil || !bytes.Equal(after, claimBefore) {
			t.Errorf("claim changed before confirmed release: %v", err)
		}
		if key, err := os.ReadFile(keyPath); err != nil || string(key) != "private" {
			t.Errorf("SSH artifact changed before confirmed release: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "synthetic-stop-token")
	var stderr bytes.Buffer
	err = (App{Stdout: io.Discard, Stderr: &stderr}).stop(t.Context(), []string{"--provider", "aws", "--id", id})
	if err != nil {
		t.Fatalf("stop: %v\n%s", err, &stderr)
	}
	if posts.Load() != 2 || observations.Load() != 2 {
		t.Fatalf("release requests=%d observations=%d; want pending observation then one completed reconciliation", posts.Load(), observations.Load())
	}
	for _, path := range []string{keyPath, claimPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("confirmed stop retained %s: %v", path, err)
		}
	}
	if strings.Contains(stderr.String(), "unexpected non-final") {
		t.Errorf("expiry cleanup was rejected: %s", &stderr)
	}
}

func TestCoordinatorStopExpiryCleanupPreservesCustody(t *testing.T) {
	for _, tc := range []struct {
		name           string
		change         func(*CoordinatorLease)
		cancel         bool
		timeout        bool
		notFound       bool
		reconciliation bool
	}{
		{name: "canceled observation", cancel: true},
		{name: "bounded observation", timeout: true},
		{name: "missing accepted record", notFound: true},
		{name: "lease replaced", change: func(l *CoordinatorLease) { l.ID = "cbx_001122334455" }},
		{name: "provider replaced", change: func(l *CoordinatorLease) { l.Provider = "hetzner" }},
		{name: "resource replaced", change: func(l *CoordinatorLease) { l.CloudID = "i-replacement" }},
		{name: "generation replaced", change: func(l *CoordinatorLease) { l.CreatedAt = "2026-10-08T01:00:00Z" }},
		{name: "cleanup claim replaced", change: func(l *CoordinatorLease) { l.State = "active"; l.CleanupStartedAt = "2026-10-08T00:00:30Z" }},
		{name: "active without cleanup claim", change: func(l *CoordinatorLease) { l.State = "active" }},
		{name: "scheduled retry", change: func(l *CoordinatorLease) {
			l.CleanupRetryAt = "2026-10-08T00:05:00Z"
			l.CleanupError = "cleanup failed"
		}},
		{name: "missing completion", change: func(l *CoordinatorLease) { l.CleanupCompletedAt = "" }},
		{name: "invalid completion", change: func(l *CoordinatorLease) { l.CleanupCompletedAt = "invalid" }},
		{name: "guest access remains", change: func(l *CoordinatorLease) { l.Host = "192.0.2.1" }},
		{name: "pending provider key", change: func(l *CoordinatorLease) { l.ProviderKeyCleanupPending = true }},
		{name: "remaining provider key identity", change: func(l *CoordinatorLease) { l.ProviderKeyCleanupID = "key-original" }},
		{name: "remaining claim deadline", change: func(l *CoordinatorLease) { l.CleanupClaimExpiresAt = "2026-10-08T00:30:00Z" }},
		{name: "remaining cleanup failure", change: func(l *CoordinatorLease) { l.CleanupFailedAt = "2026-10-08T00:00:00Z" }},
		{name: "remaining cleanup attempts", change: func(l *CoordinatorLease) { l.CleanupAttempts = 1 }},
		{name: "unsettled provisioning", change: func(l *CoordinatorLease) { l.ProvisioningRequestStartedAt = "2026-10-07T23:00:00Z" }},
		{name: "recovery pending", change: func(l *CoordinatorLease) { l.ProvisioningPhase = "interrupted-recovering" }},
		{name: "unconfirmed reconciliation", reconciliation: true},
		{name: "reconciliation resource replaced", reconciliation: true, change: func(l *CoordinatorLease) { l.CloudID = "i-replacement" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			budget := time.Second
			if tc.timeout {
				budget = 100 * time.Millisecond
			}
			configureCoordinatorReleaseTestTiming(t, budget, time.Millisecond)
			const id = "cbx_abcdef123456"
			keyPath, claimPath := managedStopLocalState(t, id)
			claimBefore, err := os.ReadFile(claimPath)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var posts, observations atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				lease := CoordinatorLease{ID: id, Provider: "aws", State: "active", CloudID: "i-original", CleanupStartedAt: "2026-10-08T00:00:00Z"}
				if r.Method == http.MethodPost {
					if posts.Add(1) > 1 {
						lease = confirmedCoordinatorRelease(id, "aws")
						lease.CloudID = "i-original"
						if tc.change != nil {
							tc.change(&lease)
						} else {
							lease.CleanupCompletedAt = ""
						}
					}
				} else if r.Method == http.MethodGet && posts.Load() > 0 {
					observations.Add(1)
					if tc.cancel {
						cancel()
						return
					}
					if tc.notFound {
						http.NotFound(w, r)
						return
					}
					if !tc.timeout {
						lease.State, lease.CleanupStartedAt, lease.CleanupCompletedAt = "expired", "", "2026-10-08T00:01:00Z"
						if tc.change != nil && !tc.reconciliation {
							tc.change(&lease)
						}
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
			}))
			defer server.Close()
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "synthetic-stop-token")
			err = (App{Stdout: io.Discard, Stderr: io.Discard}).stop(ctx, []string{"--provider", "aws", "--id", id})
			if err == nil {
				t.Fatal("unconfirmed expiry cleanup succeeded")
			}
			if tc.cancel && !errors.Is(err, context.Canceled) {
				t.Errorf("cancellation lost: %v", err)
			}
			if tc.timeout && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("deadline lost: %v", err)
			}
			if tc.cancel || tc.timeout || tc.notFound {
				if !strings.Contains(err.Error(), "crabbox status --provider aws --id "+id) || strings.Contains(err.Error(), "unexpected non-final") {
					t.Errorf("missing pending recovery guidance: %v", err)
				}
			}
			wantPosts := int32(1)
			if tc.reconciliation {
				wantPosts = 2
			}
			if posts.Load() != wantPosts || observations.Load() == 0 {
				t.Errorf("release requests=%d observations=%d; want %d requests after observation", posts.Load(), observations.Load(), wantPosts)
			}
			if after, err := os.ReadFile(claimPath); err != nil || !bytes.Equal(after, claimBefore) {
				t.Errorf("unconfirmed stop changed claim: %v", err)
			}
			if key, err := os.ReadFile(keyPath); err != nil || string(key) != "private" {
				t.Errorf("unconfirmed stop changed SSH artifacts: %v", err)
			}
		})
	}
}
