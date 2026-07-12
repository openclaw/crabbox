package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCoordinatorListUsesUserLeasesWithoutAdminProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/pool":
			t.Error("ordinary list must not probe the admin pool")
			http.Error(w, "unexpected admin probe", http.StatusInternalServerError)
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
	if stderr.Len() != 0 {
		t.Fatalf("unexpected warning: %q", stderr.String())
	}
}

func TestCoordinatorListAllFallsBackToUserLeasesWhenAdminTokenUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/pool":
			if got := r.Header.Get("Authorization"); got != "Bearer stale-admin-token" {
				t.Fatalf("pool auth=%q", got)
			}
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		case "/v1/leases":
			if got := r.Header.Get("Authorization"); got != "Bearer user-token" {
				t.Fatalf("leases auth=%q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"leases": []CoordinatorLease{
				{ID: "cbx_123", Provider: "aws", State: "active"},
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

	servers, err := backend.List(context.Background(), ListRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Labels["lease"] != "cbx_123" {
		t.Fatalf("servers=%#v", servers)
	}
	if !strings.Contains(stderr.String(), "falling back to user-visible leases") {
		t.Fatalf("missing fallback warning: %q", stderr.String())
	}
}

func TestCoordinatorListJSONUsesUserLeasesWhenAdminTokenMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/leases" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("state"); got != "active" {
			t.Fatalf("leases state=%q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"leases": []CoordinatorLease{
			{ID: "cbx_123", Provider: "daytona", State: "active", SSHUser: "daytona-live-token"},
		}})
	}))
	defer server.Close()

	cfg := Config{Provider: "daytona", TargetOS: targetLinux, Coordinator: server.URL, CoordToken: "user-token"}
	coord, _, err := newCoordinatorClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &stderr}}

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
	if leases[0].SSHUser != "<token>" {
		t.Fatalf("sshUser=%q, want redacted token", leases[0].SSHUser)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected warning: %q", stderr.String())
	}
}

func TestCoordinatorStatusRedactsDaytonaSSHAccessToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/leases/cbx_123" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
			ID:       "cbx_123",
			Provider: "daytona",
			TargetOS: targetLinux,
			SSHUser:  "daytona-live-token",
			SSHPort:  "22",
			State:    "active",
		}})
	}))
	defer server.Close()

	cfg := Config{
		Provider:    "daytona",
		TargetOS:    targetLinux,
		Coordinator: server.URL,
		CoordToken:  "user-token",
	}
	coord, _, err := newCoordinatorClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord}

	status, err := backend.Status(context.Background(), StatusRequest{ID: "cbx_123"})
	if err != nil {
		t.Fatal(err)
	}
	if status.SSHUser != "<token>" {
		t.Fatalf("sshUser=%q, want redacted token", status.SSHUser)
	}
}

func TestCoordinatorInspectJSONIncludesOptionalSSHHostKey(t *testing.T) {
	const sshHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILocalAuthoritativeHostKey"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/v1/leases/") {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		lease := CoordinatorLease{
			ID:       strings.TrimPrefix(r.URL.Path, "/v1/leases/"),
			Provider: "aws",
			TargetOS: targetLinux,
			State:    "provisioning",
		}
		if lease.ID == "cbx_with_key" {
			lease.SSHHostKey = sshHostKey
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
	}))
	defer server.Close()

	clearConfigEnv(t)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "user-token")

	for _, test := range []struct {
		id      string
		wantKey bool
	}{
		{id: "cbx_with_key", wantKey: true},
		{id: "cbx_without_key", wantKey: false},
	} {
		t.Run(test.id, func(t *testing.T) {
			var stdout bytes.Buffer
			app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
			if err := app.inspect(context.Background(), []string{"--provider", "aws", "--id", test.id, "--json"}); err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			value, ok := got["sshHostKey"]
			if test.wantKey {
				if !ok || value != sshHostKey {
					t.Fatalf("sshHostKey=%#v present=%t, want %q", value, ok, sshHostKey)
				}
			} else if ok {
				t.Fatalf("sshHostKey=%#v, want omitted", value)
			}
		})
	}
}

func TestCoordinatorAcquireSendsTailscaleHostnameTemplate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var gotHostname string
	var gotSlug string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			gotHostname, _ = body["tailscaleHostname"].(string)
			gotSlug, _ = body["slug"].(string)
			http.Error(w, `{"error":"stop after request capture"}`, http.StatusInternalServerError)
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
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.HostnameTemplate = "lease-{slug}"
	coord, _, err := newCoordinatorClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &bytes.Buffer{}}}

	if _, err := backend.acquireOnce(context.Background(), false, "smoke"); err == nil || !strings.Contains(err.Error(), "stop after request capture") {
		t.Fatalf("err=%v, want captured request error", err)
	}
	if gotSlug != "smoke" {
		t.Fatalf("slug=%q, want requested slug", gotSlug)
	}
	if gotHostname != "lease-{slug}" {
		t.Fatalf("tailscaleHostname=%q, want template for worker-side final slug render", gotHostname)
	}
}

func TestCoordinatorCreateLeaseTimesOutWithDiagnostics(t *testing.T) {
	oldTimeout := coordinatorCreateLeaseTimeoutForConfig
	oldInterval := coordinatorCreateLeaseProgressInterval
	coordinatorCreateLeaseTimeoutForConfig = func(Config) time.Duration { return 80 * time.Millisecond }
	coordinatorCreateLeaseProgressInterval = 10 * time.Millisecond
	defer func() {
		coordinatorCreateLeaseTimeoutForConfig = oldTimeout
		coordinatorCreateLeaseProgressInterval = oldInterval
	}()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases" {
			http.NotFound(w, r)
			return
		}
		time.Sleep(250 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: "cbx_timeout"}})
	}))
	defer server.Close()

	cfg := baseConfig()
	cfg.Provider = "azure"
	cfg.TargetOS = targetLinux
	cfg.ServerType = "Standard_D32ads_v6"
	cfg.Coordinator = server.URL
	cfg.CoordToken = "user-token"
	coord, _, err := newCoordinatorClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &stderr}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = backend.createCoordinatorLeaseWithProgress(ctx, cfg, "ssh-rsa test", false, "cbx_timeout", "crimson-lobster")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	for _, want := range []string{
		"timed out waiting for coordinator lease",
		"provider=azure",
		"target=linux",
		"type=Standard_D32ads_v6",
		"slug=crimson-lobster",
		"lease=cbx_timeout",
		"next_action=check coordinator/cloud logs and retry",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%q missing %q", err, want)
		}
	}
	if !strings.Contains(stderr.String(), "waiting for coordinator lease provider=azure slug=crimson-lobster") {
		t.Fatalf("missing progress output: %q", stderr.String())
	}
}

func TestCoordinatorCreateLeaseRecoversLeaseCommittedAfterCreateError(t *testing.T) {
	oldRecoveryTimeout := coordinatorCreateLeaseRecoveryTimeout
	oldRecoveryInterval := coordinatorCreateLeaseRecoveryInterval
	coordinatorCreateLeaseRecoveryTimeout = time.Second
	coordinatorCreateLeaseRecoveryInterval = 10 * time.Millisecond
	defer func() {
		coordinatorCreateLeaseRecoveryTimeout = oldRecoveryTimeout
		coordinatorCreateLeaseRecoveryInterval = oldRecoveryInterval
	}()

	var createdLeaseID string
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			createdLeaseID, _ = body["leaseID"].(string)
			http.Error(w, "error code: 1101", http.StatusInternalServerError)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/leases/"):
			gets++
			if createdLeaseID == "" || !strings.HasSuffix(r.URL.Path, createdLeaseID) {
				t.Fatalf("get path=%s created=%s", r.URL.Path, createdLeaseID)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID:                 createdLeaseID,
				Slug:               "jade-crab",
				Provider:           "azure",
				TargetOS:           targetWindows,
				WindowsMode:        windowsModeNormal,
				CloudID:            "crabbox-jade-crab",
				Host:               "203.0.113.44",
				SSHUser:            "crabbox",
				SSHPort:            "22",
				WorkRoot:           defaultWindowsWorkRoot,
				State:              "active",
				ServerType:         "Standard_D2ads_v6",
				IdleTimeoutSeconds: 1800,
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := baseConfig()
	cfg.Provider = "azure"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	cfg.Coordinator = server.URL
	cfg.CoordToken = "user-token"
	coord, _, err := newCoordinatorClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &stderr}}

	lease, err := backend.createCoordinatorLeaseWithProgress(context.Background(), cfg, "ssh-rsa test", false, "cbx_recover", "jade-crab")
	if err != nil {
		t.Fatal(err)
	}
	if lease.ID != createdLeaseID || lease.Host != "203.0.113.44" {
		t.Fatalf("lease=%#v created=%s", lease, createdLeaseID)
	}
	if gets == 0 {
		t.Fatal("expected recovery GET")
	}
	for _, want := range []string{
		"uncertain result",
		"recovered coordinator lease",
		createdLeaseID,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr=%q missing %q", stderr.String(), want)
		}
	}
}

func TestLeaseToServerTargetPreservesCoordinatorWorkRoot(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetLinux
	cfg.WorkRoot = defaultPOSIXWorkRoot

	server, target, leaseID := leaseToServerTarget(CoordinatorLease{
		ID:         "cbx_123",
		Slug:       "silver-squid",
		Provider:   "aws",
		TargetOS:   targetMacOS,
		HostID:     "h-000000000001",
		SSHUser:    "ec2-user",
		SSHPort:    "22",
		Host:       "203.0.113.10",
		SSHHostKey: "ssh-ed25519 AAAAauthoritative",
		WorkRoot:   defaultMacOSWorkRoot,
		ServerType: "mac2.metal",
		State:      "active",
	}, cfg)

	if leaseID != "cbx_123" {
		t.Fatalf("leaseID=%q", leaseID)
	}
	if target.TargetOS != targetMacOS || target.User != "ec2-user" || target.Port != "22" {
		t.Fatalf("target=%#v", target)
	}
	if target.SSHHostKey != "ssh-ed25519 AAAAauthoritative" {
		t.Fatalf("ssh host key=%q", target.SSHHostKey)
	}
	if server.Labels["work_root"] != defaultMacOSWorkRoot {
		t.Fatalf("work_root label=%q want %q", server.Labels["work_root"], defaultMacOSWorkRoot)
	}
	if server.HostID != "h-000000000001" || server.Labels["host_id"] != "h-000000000001" {
		t.Fatalf("server host id not preserved: %#v", server)
	}

	applyResolvedServerConfig(&cfg, server)
	if cfg.WorkRoot != defaultMacOSWorkRoot {
		t.Fatalf("workRoot=%q want %q", cfg.WorkRoot, defaultMacOSWorkRoot)
	}
}

func TestLeaseToServerTargetMarksDaytonaSSHUserSecret(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "daytona"
	cfg.SSHKey = "/tmp/must-not-be-used"

	_, target, _ := leaseToServerTarget(CoordinatorLease{
		ID:       "cbx_123",
		Provider: "daytona",
		SSHUser:  "daytona-secret-token",
		SSHPort:  "22",
		Host:     "ssh.app.daytona.io",
	}, cfg)

	if target.User != "daytona-secret-token" || target.Key != "" || !target.AuthSecret {
		t.Fatalf("target=%#v", target)
	}
	if target.ReadyCheck != "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null" {
		t.Fatalf("ready check=%q", target.ReadyCheck)
	}
	if target.NetworkKind != NetworkPublic {
		t.Fatalf("network kind=%q want public", target.NetworkKind)
	}
}

func TestLeaseToServerTargetPreservesPondExposedPorts(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"

	server, _, _ := leaseToServerTarget(CoordinatorLease{
		ID:           "cbx_123",
		Slug:         "web",
		Provider:     "aws",
		Pond:         "alpha",
		ExposedPorts: []string{"8080", "9090"},
		Host:         "203.0.113.10",
	}, cfg)

	if server.Labels[pondLabelKey] != "alpha" {
		t.Fatalf("pond label=%q want alpha", server.Labels[pondLabelKey])
	}
	if server.Labels[pondExposedPortsLabelKey] != "8080-9090" {
		t.Fatalf("exposed ports label=%q want 8080-9090", server.Labels[pondExposedPortsLabelKey])
	}
}

func TestLeaseToServerTargetPreservesDesktopEnvLabel(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetLinux

	server, _, _ := leaseToServerTarget(CoordinatorLease{
		ID:         "cbx_123",
		Provider:   "aws",
		TargetOS:   targetLinux,
		Desktop:    true,
		DesktopEnv: desktopEnvWayland,
		Host:       "203.0.113.10",
	}, cfg)

	if server.Labels["desktop_env"] != desktopEnvWayland {
		t.Fatalf("desktop_env label=%q want %q", server.Labels["desktop_env"], desktopEnvWayland)
	}
}

func TestCoordinatorLeaseHostIDAcceptsCanonicalAndCompatJSON(t *testing.T) {
	for name, input := range map[string]string{
		"canonical": `{"id":"cbx_123","provider":"aws","hostId":"h-canonical"}`,
		"compat":    `{"id":"cbx_123","provider":"aws","hostID":"h-compat"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var lease CoordinatorLease
			if err := json.Unmarshal([]byte(input), &lease); err != nil {
				t.Fatal(err)
			}
			if got := coordinatorLeaseHostID(lease); got == "" || !strings.HasPrefix(got, "h-") {
				t.Fatalf("host id not decoded from %s: %#v", name, lease)
			}
		})
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

	_, err = backend.acquireOnce(context.Background(), false, "")
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

	_, err = backend.acquireOnce(context.Background(), false, "")
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
