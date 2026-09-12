package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCoordinatorAcquireRejectsUnrelatedLeaseBeforeBootstrapOrCleanup(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		for _, kept := range []bool{false, true} {
			t.Run(fmt.Sprintf("fixed=%v/kept=%v", fixed, kept), func(t *testing.T) {
				isolateTestUserDirs(t)
				const heldID = "cbx_111111111111"
				heldKey, _, err := ensureTestboxKeyForConfig(baseConfig(), heldID)
				if err != nil {
					t.Fatal(err)
				}
				original, err := os.ReadFile(heldKey)
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if calls != 1 || (r.Method != http.MethodPost && r.Method != http.MethodPut) || strings.Contains(r.URL.Path, "release") || strings.Contains(r.URL.Path, "cancel-create") {
						t.Errorf("unexpected request after mismatched create: %s %s", r.Method, r.URL.Path)
						http.Error(w, "unexpected", http.StatusBadRequest)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
						ID: heldID, Slug: "kept-desktop", Provider: "aws", TargetOS: targetMacOS,
						Keep: kept, State: "active", CloudID: "i-kept", ServerID: 0,
						Host: "127.0.0.1", SSHPort: "1", SSHUser: "crabbox",
					}})
				}))
				defer server.Close()
				cfg := baseConfig()
				cfg.Provider, cfg.TargetOS = "aws", targetMacOS
				cfg.Coordinator, cfg.CoordToken = server.URL, "synthetic-token"
				cfg.AWSSSHCIDRs = []string{"203.0.113.7/32"}
				var stderr bytes.Buffer
				backend := &coordinatorLeaseBackend{cfg: cfg, coord: mustNewCoordinatorClient(t, cfg), rt: Runtime{Stderr: &stderr}}
				req := AcquireRequest{Keep: true, RequestedSlug: "new-warmup", OnAcquired: func(LeaseTarget) error { t.Error("unexpected acquisition"); return nil }}
				if fixed {
					req.RequestedLeaseID = "cbx_222222222222"
				}
				_, err = backend.Acquire(t.Context(), req)
				if !isCoordinatorLeaseIDConflict(err) || !strings.Contains(err.Error(), heldID) || !strings.Contains(err.Error(), "kept-desktop") {
					t.Fatalf("err=%v", err)
				}
				if calls != 1 || strings.Contains(stderr.String(), "leased "+heldID) {
					t.Fatalf("calls=%d stderr=%s", calls, &stderr)
				}
				after, err := os.ReadFile(heldKey)
				if err != nil || !bytes.Equal(after, original) {
					t.Fatal("unrelated lease key changed")
				}
			})
		}
	}
}

func TestCoordinatorCreateIdentityMismatchNeverCancelsLegacyAdoption(t *testing.T) {
	for _, stage := range []string{"initial", "recovery", "activation"} {
		t.Run(stage, func(t *testing.T) {
			oldInterval := coordinatorCreateLeaseRecoveryInterval
			coordinatorCreateLeaseRecoveryInterval = time.Millisecond
			defer func() { coordinatorCreateLeaseRecoveryInterval = oldInterval }()
			const requestedID = "cbx_222222222222"
			creates, gets, mutations := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				lease := CoordinatorLease{ID: "cbx_111111111111", Slug: "kept-desktop", Keep: true, Provider: "aws", TargetOS: targetMacOS, State: "active", Host: "127.0.0.1"}
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
					creates++
					if stage == "recovery" && creates == 1 {
						http.Error(w, "uncertain", http.StatusInternalServerError)
						return
					}
					if stage == "activation" {
						lease.ID, lease.State, lease.Host = requestedID, "provisioning", ""
					}
				case r.Method == http.MethodGet:
					gets++
				default:
					mutations++
					http.Error(w, "must not cancel a cross-ID binding", http.StatusBadRequest)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
			}))
			defer server.Close()
			cfg := baseConfig()
			cfg.Provider, cfg.TargetOS = "aws", targetMacOS
			cfg.Coordinator, cfg.CoordToken = server.URL, "synthetic-token"
			backend := &coordinatorLeaseBackend{cfg: cfg, coord: mustNewCoordinatorClient(t, cfg), rt: Runtime{Stderr: &bytes.Buffer{}}}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, err := backend.createCoordinatorLeaseWithProgress(ctx, cfg, "ssh-ed25519 synthetic", true, requestedID, "new-warmup")
			if !isCoordinatorLeaseIDConflict(err) || mutations != 0 {
				t.Fatalf("err=%v mutations=%d", err, mutations)
			}
			if stage == "activation" && gets != 1 {
				t.Fatalf("gets=%d", gets)
			}
			if stage == "recovery" && creates != 2 {
				t.Fatalf("creates=%d", creates)
			}
		})
	}
}

func TestCoordinatorListIncludesRetainedLeasesInTextAndJSON(t *testing.T) {
	retained, deleted := false, true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "" || r.URL.Query().Get("view") != "current" || r.URL.Query().Get("provider") != "aws" {
			t.Errorf("query=%s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"leases": []CoordinatorLease{
			{ID: "cbx_active", Provider: "aws", State: "active"},
			{ID: "cbx_kept", Slug: "kept-desktop", Provider: "aws", State: "released", Keep: true, ReleaseDeletesServer: &retained},
			{ID: "cbx_retained", Provider: "aws", State: "released", ReleaseDeletesServer: &retained},
			{ID: "cbx_deleted", Provider: "aws", State: "released", Keep: true, ReleaseDeletesServer: &deleted, CleanupStatus: "complete", CleanupCompletedAt: "2026-01-01T00:00:00Z"},
			{ID: "cbx_ended", Provider: "aws", State: "expired"},
			{ID: "cbx_other", Provider: "azure", State: "active", Keep: true},
		}})
	}))
	defer server.Close()
	cfg := baseConfig()
	cfg.Provider, cfg.Coordinator, cfg.CoordToken = "aws", server.URL, "synthetic-token"
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: mustNewCoordinatorClient(t, cfg), rt: Runtime{Stderr: &bytes.Buffer{}}}
	servers, err := backend.List(t.Context(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 3 || servers[1].Labels["lease"] != "cbx_kept" || servers[1].Labels["slug"] != "kept-desktop" {
		t.Fatalf("servers=%#v", servers)
	}
	result, err := backend.ListJSON(t.Context(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	leases := result.([]CoordinatorLease)
	if len(leases) != 3 || leases[1].ID != "cbx_kept" || !leases[1].Keep {
		t.Fatalf("leases=%#v", leases)
	}
}
