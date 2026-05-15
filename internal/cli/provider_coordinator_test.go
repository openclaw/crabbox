package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCoordinatorListFallsBackToUserLeasesWhenAdminTokenUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/pool":
			if got := r.Header.Get("Authorization"); got != "Bearer stale-admin-token" {
				t.Fatalf("pool auth=%q", got)
			}
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		case "/v1/leases":
			if got := r.URL.Query().Get("state"); got != "active" {
				t.Fatalf("leases state=%q", got)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer user-token" {
				t.Fatalf("leases auth=%q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"leases": []CoordinatorLease{
				{
					ID:                 "cbx_123",
					Slug:               "blue-lobster",
					Provider:           "aws",
					TargetOS:           targetLinux,
					ServerID:           42,
					CloudID:            "i-123",
					ServerName:         "crabbox-blue-lobster",
					Host:               "203.0.113.10",
					SSHUser:            "crabbox",
					SSHPort:            "2222",
					ServerType:         "c7a.48xlarge",
					State:              "active",
					Keep:               true,
					ExpiresAt:          "2026-05-07T15:00:00Z",
					IdleTimeoutSeconds: 1800,
				},
				{ID: "cbx_other", Provider: "hetzner", State: "active"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stderr bytes.Buffer
	cfg := Config{
		Provider:        "aws",
		TargetOS:        targetLinux,
		Coordinator:     server.URL,
		CoordToken:      "user-token",
		CoordAdminToken: "stale-admin-token",
	}
	coord, _, err := newCoordinatorClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &stderr}}

	servers, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 {
		t.Fatalf("servers=%d, want 1: %#v", len(servers), servers)
	}
	if servers[0].Labels["lease"] != "cbx_123" || servers[0].Labels["slug"] != "blue-lobster" {
		t.Fatalf("server labels=%#v", servers[0].Labels)
	}
	if !strings.Contains(stderr.String(), "falling back to user-visible leases") {
		t.Fatalf("missing fallback warning: %q", stderr.String())
	}
}

func TestCoordinatorListJSONFallsBackWhenAdminTokenMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/leases" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("state"); got != "active" {
			t.Fatalf("leases state=%q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"leases": []CoordinatorLease{
			{ID: "cbx_123", Provider: "aws", State: "active"},
		}})
	}))
	defer server.Close()

	cfg := Config{Provider: "aws", TargetOS: targetLinux, Coordinator: server.URL, CoordToken: "user-token"}
	coord, _, err := newCoordinatorClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &bytes.Buffer{}}}

	view, err := backend.ListJSON(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	leases, ok := view.([]CoordinatorLease)
	if !ok {
		t.Fatalf("view=%T, want []CoordinatorLease", view)
	}
	if len(leases) != 1 || leases[0].ID != "cbx_123" {
		t.Fatalf("leases=%#v", leases)
	}
}

func TestCoordinatorResolveFallsBackToAdminToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/leases/cbx_admin" {
			http.NotFound(w, r)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer user-token":
			http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
		case "Bearer admin-token":
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID:                 "cbx_admin",
				Slug:               "green-shrimp",
				Provider:           "aws",
				TargetOS:           targetLinux,
				CloudID:            "i-admin",
				Host:               "203.0.113.44",
				SSHUser:            "crabbox",
				SSHPort:            "2222",
				SSHFallbackPorts:   []string{"22"},
				WorkRoot:           "/work/crabbox",
				State:              "active",
				ServerType:         "t3.small",
				IdleTimeoutSeconds: 600,
			}})
		default:
			t.Fatalf("unexpected auth %q", r.Header.Get("Authorization"))
		}
	}))
	defer server.Close()

	cfg := Config{
		Provider:        "aws",
		TargetOS:        targetLinux,
		Coordinator:     server.URL,
		CoordToken:      "user-token",
		CoordAdminToken: "admin-token",
	}
	coord, _, err := newCoordinatorClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &bytes.Buffer{}}}

	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "cbx_admin"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_admin" || lease.SSH.Host != "203.0.113.44" || lease.Coordinator == nil {
		t.Fatalf("lease=%#v", lease)
	}
	if lease.Coordinator.Token != "admin-token" {
		t.Fatalf("coordinator token=%q, want admin token", lease.Coordinator.Token)
	}
}

func TestCoordinatorReleaseFallsBackToAdminToken(t *testing.T) {
	adminReleased := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/admin/leases/cbx_admin/release" && r.URL.Path != "/v1/leases/cbx_admin/release" {
			http.NotFound(w, r)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer user-token":
			http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
		case "Bearer admin-token":
			if r.URL.Path != "/v1/admin/leases/cbx_admin/release" {
				t.Fatalf("admin release path=%s", r.URL.Path)
			}
			adminReleased = true
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: "cbx_admin", Provider: "aws", State: "released"}})
		default:
			t.Fatalf("unexpected auth %q", r.Header.Get("Authorization"))
		}
	}))
	defer server.Close()

	cfg := Config{
		Provider:        "aws",
		TargetOS:        targetLinux,
		Coordinator:     server.URL,
		CoordToken:      "user-token",
		CoordAdminToken: "admin-token",
	}
	coord, _, err := newCoordinatorClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &bytes.Buffer{}}}

	err = backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: "cbx_admin"}})
	if err != nil {
		t.Fatal(err)
	}
	if !adminReleased {
		t.Fatal("admin release was not called")
	}
}

func TestCoordinatorAcquireReleasesStaleInstanceLease(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var createdLeaseID string
	releases := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			createdLeaseID, _ = body["leaseID"].(string)
			http.Error(w, `{"error":"InvalidInstanceID.NotFound: instance disappeared"}`, http.StatusInternalServerError)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/release"):
			releases++
			if createdLeaseID == "" || !strings.Contains(r.URL.Path, createdLeaseID) {
				t.Fatalf("release path=%s created=%s", r.URL.Path, createdLeaseID)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID:       createdLeaseID,
				Provider: "aws",
				State:    "released",
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetLinux
	cfg.Coordinator = server.URL
	cfg.CoordToken = "user-token"
	coord, _, err := newCoordinatorClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &stderr}}

	_, err = backend.acquireOnce(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "InvalidInstanceID.NotFound") {
		t.Fatalf("err=%v", err)
	}
	if !isCoordinatorStaleInstanceCleanedError(err) {
		t.Fatalf("err=%T, want cleaned stale instance wrapper", err)
	}
	if releases != 1 {
		t.Fatalf("releases=%d want 1", releases)
	}
	if !strings.Contains(stderr.String(), "discarded stale coordinator lease") {
		t.Fatalf("missing discard warning: %q", stderr.String())
	}
}

func TestCoordinatorAcquireRetriesStaleInstanceWhenReleaseMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	creates := 0
	releases := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
			creates++
			if creates > 1 {
				http.Error(w, `{"error":"capacity exhausted after retry"}`, http.StatusInternalServerError)
				return
			}
			http.Error(w, `{"error":"InvalidInstanceID.NotFound: instance disappeared"}`, http.StatusInternalServerError)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/release"):
			releases++
			http.Error(w, `{"error":"lease not found"}`, http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetLinux
	cfg.Coordinator = server.URL
	cfg.CoordToken = "user-token"
	coord, _, err := newCoordinatorClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &stderr}}

	_, err = backend.Acquire(context.Background(), AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "capacity exhausted after retry") {
		t.Fatalf("err=%v", err)
	}
	if creates != 2 {
		t.Fatalf("creates=%d want 2", creates)
	}
	if releases != 1 {
		t.Fatalf("releases=%d want 1", releases)
	}
	if !strings.Contains(stderr.String(), "already gone; retrying with fresh lease") {
		t.Fatalf("missing retry warning: %q", stderr.String())
	}
}

func TestCoordinatorAcquireWrapsWorkerCleanupSignalWithoutRelease(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	creates := 0
	releases := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
			creates++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			http.Error(w, `{"error":"InvalidInstanceID.NotFound: instance disappeared; crabbox_aws_stale_instance_cleaned; deleted AWS instance i-stale after readiness failure"}`, http.StatusInternalServerError)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/release"):
			releases++
			http.Error(w, `{"error":"lease not found"}`, http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetLinux
	cfg.Coordinator = server.URL
	cfg.CoordToken = "user-token"
	cfg.AWSSSHCIDRs = []string{"0.0.0.0/0"}
	coord, _, err := newCoordinatorClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &stderr}}

	_, err = backend.acquireOnce(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "InvalidInstanceID.NotFound") {
		t.Fatalf("err=%v", err)
	}
	if !isCoordinatorStaleInstanceCleanedError(err) {
		t.Fatalf("err=%T, want cleaned stale instance wrapper", err)
	}
	if creates != 1 {
		t.Fatalf("creates=%d want 1", creates)
	}
	if releases != 0 {
		t.Fatalf("releases=%d want 0", releases)
	}
}
