package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type coordinatorAcquireValidationBackend struct {
	testSSHBackend
	err   error
	calls int
}

func (b *coordinatorAcquireValidationBackend) ValidateCoordinatorAcquire() error {
	b.calls++
	return b.err
}

func TestCoordinatorAcquireValidatesBeforeAllocation(t *testing.T) {
	t.Parallel()
	for _, requestedID := range []string{"", "cbx_0123456789ab"} {
		t.Run(blank(requestedID, "generated-id"), func(t *testing.T) {
			cause := errors.New("provider allocation is unsupported")
			direct := &coordinatorAcquireValidationBackend{err: cause}
			// No client or key configuration: rejected acquisition must not reach
			// allocation, including the fixed-ID and retry paths.
			backend := &coordinatorLeaseBackend{direct: direct}
			if _, err := backend.Acquire(t.Context(), AcquireRequest{RequestedLeaseID: requestedID}); !errors.Is(err, cause) || direct.calls != 1 {
				t.Fatalf("calls=%d err=%v", direct.calls, err)
			}
		})
	}
}

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
			{
				ID:              "cbx_123",
				Provider:        "daytona",
				State:           "provisioning",
				SSHUser:         "daytona-live-token",
				CleanupAttempts: 2,
				CleanupError:    "provider resource not yet visible",
				CleanupRetryAt:  "2026-08-03T11:05:00Z",
				FailureError:    "interrupted provisioning",
			},
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
	if leases[0].CleanupAttempts != 2 ||
		leases[0].CleanupError != "provider resource not yet visible" ||
		leases[0].CleanupRetryAt != "2026-08-03T11:05:00Z" ||
		leases[0].FailureError != "interrupted provisioning" {
		t.Fatalf("recovery diagnostics were not preserved: %#v", leases[0])
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
		if r.URL.RawQuery != "" {
			t.Fatalf("ordinary status unexpectedly requested provider metadata: %s", r.URL.RawQuery)
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

func TestCoordinatorStatusKeepsFourSecondWindowsSSHProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake ssh helper is only reliable on Unix hosts")
	}
	isolateTestUserDirs(t)
	logPath := installSSHArgsRecorder(t)
	t.Setenv("CRABBOX_FAKE_SSH_DELAY", "4.2")
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/leases/cbx_windows_status" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
			ID:          "cbx_windows_status",
			Provider:    "aws",
			TargetOS:    targetWindows,
			WindowsMode: windowsModeNormal,
			Host:        host,
			SSHUser:     "crabbox",
			SSHPort:     port,
			State:       "active",
		}})
	}))
	defer server.Close()

	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	cfg.Network = NetworkPublic
	cfg.Coordinator = server.URL
	cfg.CoordToken = "user-token"
	coord, _, err := newCoordinatorClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord}
	start := time.Now()
	status, err := backend.Status(context.Background(), StatusRequest{ID: "cbx_windows_status"})
	if err != nil {
		t.Fatal(err)
	}
	if status.Ready {
		t.Fatal("status Ready=true, want false when Windows SSH exceeds the 4s status budget")
	}
	if elapsed := time.Since(start); elapsed < 3500*time.Millisecond || elapsed > 6*time.Second {
		t.Fatalf("coordinator status probe elapsed=%s, want approximately 4s", elapsed)
	}
	args := readSSHArgsRecorder(t, logPath)
	assertSSHOption(t, args, "ConnectTimeout", "10")
	assertSSHOption(t, args, "ConnectionAttempts", "3")
}

func TestCoordinatorInspectJSONIncludesOptionalSSHHostKey(t *testing.T) {
	isolateTestUserDirs(t)
	sshHostKey := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 47))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/v1/leases/") {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("providerMetadata"); got != "authoritative" {
			t.Fatalf("providerMetadata=%q, want authoritative", got)
		}
		lease := CoordinatorLease{
			ID:               strings.TrimPrefix(r.URL.Path, "/v1/leases/"),
			Slug:             "blue-lobster",
			Provider:         "aws",
			TargetOS:         targetLinux,
			Pond:             "evaluation",
			ServerType:       "c7a.large",
			Market:           "on-demand",
			Keep:             false,
			State:            "provisioning",
			ProviderMetadata: map[string]any{"instanceProfileAttached": false},
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
		id             string
		configuredWith string
		wantKey        bool
	}{
		{id: "cbx_with_key", configuredWith: "aws", wantKey: true},
		{id: "cbx_without_key", configuredWith: "aws", wantKey: false},
	} {
		t.Run(test.id, func(t *testing.T) {
			var stdout bytes.Buffer
			app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
			if err := app.inspect(context.Background(), []string{"--provider", test.configuredWith, "--id", test.id, "--json"}); err != nil {
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
			metadata, ok := got["providerMetadata"].(map[string]any)
			if !ok || metadata["instanceProfileAttached"] != false {
				t.Fatalf("providerMetadata=%#v, want authoritative false", got["providerMetadata"])
			}
			keyPath, err := testboxKeyPath(test.id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Dir(keyPath)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("metadata-only inspect created SSH trust state: %v", err)
			}
			labels, ok := got["labels"].(map[string]any)
			if !ok {
				t.Fatalf("labels=%#v, want object", got["labels"])
			}
			for key, want := range map[string]any{
				"lease":       test.id,
				"slug":        "blue-lobster",
				"keep":        "false",
				"target":      targetLinux,
				"pond":        "evaluation",
				"provider":    "aws",
				"server_type": "c7a.large",
				"market":      "on-demand",
			} {
				if labels[key] != want {
					t.Fatalf("labels[%q]=%#v, want %#v; labels=%#v", key, labels[key], want, labels)
				}
			}
		})
	}
}

func TestCoordinatorInspectJSONPreservesCleanupState(t *testing.T) {
	isolateTestUserDirs(t)
	retained := false
	deleting := true
	releaseDeletesServerByID := map[string]*bool{
		"cbx_unspecified": nil,
		"cbx_retained":    &retained,
		"cbx_deleting":    &deleting,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/v1/leases/") {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		leaseID := strings.TrimPrefix(r.URL.Path, "/v1/leases/")
		releaseDeletesServer, ok := releaseDeletesServerByID[leaseID]
		if !ok {
			t.Fatalf("unexpected lease %q", leaseID)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
			ID:                   leaseID,
			Provider:             "aws",
			TargetOS:             targetLinux,
			State:                "released",
			CleanupStartedAt:     "2026-08-24T08:00:00Z",
			CleanupError:         "cleanup failed",
			CleanupRetryAt:       "2026-08-24T08:05:00Z",
			ReleaseDeletesServer: releaseDeletesServer,
		}})
	}))
	defer server.Close()

	clearConfigEnv(t)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "user-token")

	for _, test := range []struct {
		id                       string
		wantReleaseDeletesServer *bool
	}{
		{id: "cbx_unspecified"},
		{id: "cbx_retained", wantReleaseDeletesServer: &retained},
		{id: "cbx_deleting", wantReleaseDeletesServer: &deleting},
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
			for field, want := range map[string]any{
				"cleanupStartedAt": "2026-08-24T08:00:00Z",
				"cleanupError":     "cleanup failed",
				"cleanupRetryAt":   "2026-08-24T08:05:00Z",
			} {
				if got[field] != want {
					t.Fatalf("%s=%#v, want %#v", field, got[field], want)
				}
			}
			releaseDeletesServer, present := got["releaseDeletesServer"]
			if test.wantReleaseDeletesServer == nil {
				if present {
					t.Fatalf("releaseDeletesServer=%#v, want omitted", releaseDeletesServer)
				}
				return
			}
			if !present || releaseDeletesServer != *test.wantReleaseDeletesServer {
				t.Fatalf("releaseDeletesServer=%#v present=%t, want %t", releaseDeletesServer, present, *test.wantReleaseDeletesServer)
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
			http.Error(w, `{"error":"stop after request capture"}`, http.StatusBadRequest)
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

func TestCoordinatorAcquirePreservesAWSSSHCIDROwnership(t *testing.T) {
	previousDetector := detectOutboundIPv4CIDRFunc
	defer func() { detectOutboundIPv4CIDRFunc = previousDetector }()

	for _, test := range []struct {
		name           string
		cidrs          []string
		wantCIDRs      []string
		detectionError error
		wantDetections int
		pinned         bool
	}{
		{
			name:           "unpinned request preserves detected outbound IPv4",
			wantCIDRs:      []string{"192.0.2.44/32"},
			wantDetections: 1,
		},
		{
			name:           "unpinned request tolerates unavailable outbound IPv4",
			detectionError: errors.New("outbound IPv4 unavailable"),
			wantDetections: 1,
		},
		{
			name:      "explicit CIDRs stay pinned",
			cidrs:     []string{"198.51.100.7/32", "203.0.113.0/24"},
			wantCIDRs: []string{"198.51.100.7/32", "203.0.113.0/24"},
			pinned:    true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			detections := 0
			detectOutboundIPv4CIDRFunc = func(context.Context) (string, error) {
				detections++
				return "192.0.2.44/32", test.detectionError
			}

			var body struct {
				CIDRs  []string `json:"awsSSHCIDRs"`
				Pinned bool     `json:"awsSSHCIDRsPinned"`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/leases" {
					http.NotFound(w, r)
					return
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				http.Error(w, `{"error":"stop after request capture"}`, http.StatusBadRequest)
			}))
			defer server.Close()

			cfg := baseConfig()
			cfg.Provider = "aws"
			cfg.TargetOS = targetLinux
			cfg.Coordinator = server.URL
			cfg.CoordToken = "user-token"
			cfg.AWSSSHCIDRs = test.cidrs
			coord, _, err := newCoordinatorClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: io.Discard}}
			if _, err := backend.acquireOnce(context.Background(), false, "cidr-source"); err == nil || !strings.Contains(err.Error(), "stop after request capture") {
				t.Fatalf("err=%v, want captured request error", err)
			}
			if detections != test.wantDetections {
				t.Fatalf("coordinator outbound-IP detections=%d, want %d", detections, test.wantDetections)
			}
			if !slices.Equal(body.CIDRs, test.wantCIDRs) || body.Pinned != test.pinned {
				t.Fatalf("awsSSHCIDRs=%v awsSSHCIDRsPinned=%t, want %v and %t", body.CIDRs, body.Pinned, test.wantCIDRs, test.pinned)
			}
		})
	}
}

func TestCoordinatorFixedAcquireUsesRequestedIDAndJoinsProvisioning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	oldInterval := coordinatorCreateLeaseRecoveryInterval
	coordinatorCreateLeaseRecoveryInterval = time.Millisecond
	defer func() { coordinatorCreateLeaseRecoveryInterval = oldInterval }()

	puts := 0
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/leases/cbx_abcdef123456":
			puts++
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: "cbx_abcdef123456", Slug: "fixed-coordinator", Provider: "aws", TargetOS: targetLinux, State: "provisioning",
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/cbx_abcdef123456":
			gets++
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: "cbx_abcdef123456", Slug: "fixed-coordinator", Provider: "aws", TargetOS: targetLinux,
				State: "active", CloudID: "i-fixed", Host: "203.0.113.10", SSHUser: "crabbox", SSHPort: "2222", WorkRoot: defaultPOSIXWorkRoot,
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
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &bytes.Buffer{}}}
	lease, err := backend.createCoordinatorLeaseWithProgressMode(
		context.Background(), cfg, "ssh-ed25519 test", true,
		"cbx_abcdef123456", "fixed-coordinator", true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if lease.ID != "cbx_abcdef123456" || lease.CloudID != "i-fixed" || puts != 1 || gets == 0 {
		t.Fatalf("lease=%#v puts=%d gets=%d", lease, puts, gets)
	}
}

func TestCoordinatorAcquireRetainsCurrentProvisioningTiming(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake ssh helper requires a unix-like host")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	toolDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(toolDir, "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", toolDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	readyServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer readyServer.Close()
	readyURL, err := url.Parse(readyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(readyURL.Host)
	if err != nil {
		t.Fatal(err)
	}

	lease := CoordinatorLease{
		ID: "cbx_abcdef123468", Slug: "current-timing", Provider: "aws", TargetOS: targetLinux,
		State: "active", CloudID: "i-current", Host: host, SSHUser: "crabbox", SSHPort: port, WorkRoot: defaultPOSIXWorkRoot,
		ProvisioningTiming: &CoordinatorProvisioningTiming{
			RequestMs: 2,
			TotalMs:   5,
			Phases: []CoordinatorProvisioningPhase{
				{Name: "request", Ms: 2},
				{Name: "unattributed", Ms: 3},
			},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/leases/"+lease.ID:
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+lease.ID:
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/"+lease.ID+"/heartbeat":
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
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
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: io.Discard}}
	acquired, err := backend.Acquire(context.Background(), AcquireRequest{
		Keep: true, RequestedLeaseID: lease.ID, RequestedSlug: lease.Slug,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := coordinatorRunnerTiming(lease)
	if !reflect.DeepEqual(acquired.runnerTiming, want) {
		t.Fatalf("runner timing=%#v want current provisioning timing %#v", acquired.runnerTiming, want)
	}
}

func TestCoordinatorAcquirePollsCanonicalIDFromProvisioningReplay(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	oldInterval := coordinatorCreateLeaseRecoveryInterval
	coordinatorCreateLeaseRecoveryInterval = time.Millisecond
	defer func() { coordinatorCreateLeaseRecoveryInterval = oldInterval }()

	const requestedID = "cbx_abcdef123456"
	const canonicalID = "cbx_abcdef123457"
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: canonicalID, Slug: "retained-canonical", Provider: "aws", TargetOS: targetLinux, State: "provisioning",
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+canonicalID:
			gets++
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: canonicalID, Slug: "retained-canonical", Provider: "aws", TargetOS: targetLinux,
				State: "active", CloudID: "i-retained", Host: "203.0.113.10", SSHUser: "crabbox", SSHPort: "2222", WorkRoot: defaultPOSIXWorkRoot,
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+requestedID:
			t.Fatalf("polled provisional ID instead of canonical ID")
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
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &bytes.Buffer{}}}
	lease, err := backend.createCoordinatorLeaseWithProgressMode(
		context.Background(), cfg, "ssh-ed25519 test", true, requestedID, "retained-canonical", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if lease.ID != canonicalID || lease.CloudID != "i-retained" || gets == 0 {
		t.Fatalf("lease=%#v gets=%d", lease, gets)
	}
}

func TestCoordinatorFixedAcquireInvokesOnAcquiredOnceAndPropagatesError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake ssh helper requires a unix-like host")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	toolDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(toolDir, "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", toolDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	readyServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer readyServer.Close()
	readyURL, err := url.Parse(readyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(readyURL.Host)
	if err != nil {
		t.Fatal(err)
	}

	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lease := CoordinatorLease{
			ID: "cbx_abcdef123467", Slug: "fixed-callback", Provider: "aws", TargetOS: targetLinux,
			State: "active", CloudID: "i-fixed", Host: host, SSHUser: "crabbox", SSHPort: port, WorkRoot: defaultPOSIXWorkRoot,
		}
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/leases/cbx_abcdef123467":
			creates++
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/cbx_abcdef123467":
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/cbx_abcdef123467/heartbeat":
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
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
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &bytes.Buffer{}}}
	want := errors.New("acknowledgment rejected")
	callbacks := 0
	lease, err := backend.Acquire(context.Background(), AcquireRequest{
		Keep: true, RequestedLeaseID: "cbx_abcdef123467", RequestedSlug: "fixed-callback",
		OnAcquired: func(acquired LeaseTarget) error {
			callbacks++
			if acquired.LeaseID != "cbx_abcdef123467" || acquired.Server.CloudID != "i-fixed" {
				t.Fatalf("callback acquired=%#v", acquired)
			}
			return want
		},
	})
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "acknowledge fixed coordinator acquisition") {
		t.Fatalf("lease=%#v err=%v", lease, err)
	}
	if lease.LeaseID != "" {
		t.Fatalf("error returned non-empty lease=%#v", lease)
	}
	if callbacks != 1 || creates != 1 {
		t.Fatalf("callbacks=%d creates=%d, want one each", callbacks, creates)
	}
}

func TestCoordinatorFixedAcquireDoesNotReleaseCommittedLeaseAfterClientBootstrapFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	releases := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/leases/cbx_abcdef123457":
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: "cbx_abcdef123457", Slug: "fixed-bootstrap", Provider: "aws", TargetOS: targetLinux,
				State: "active", CloudID: "i-fixed", Host: "127.0.0.1", SSHUser: "crabbox", SSHPort: "1", WorkRoot: defaultPOSIXWorkRoot,
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/cbx_abcdef123457/release":
			releases++
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: "cbx_abcdef123457", State: "released"}})
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
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &bytes.Buffer{}}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = backend.Acquire(ctx, AcquireRequest{
		Keep: true, RequestedLeaseID: "cbx_abcdef123457", RequestedSlug: "fixed-bootstrap",
	})
	if err == nil {
		t.Fatal("expected client-side bootstrap failure")
	}
	if releases != 0 {
		t.Fatalf("fixed replay released committed coordinator lease %d time(s)", releases)
	}
}

func TestCoordinatorAcquireReportsSelectedImageAndProviderStartupTiming(t *testing.T) {
	lease := CoordinatorLease{
		ID:         "cbx_image",
		Slug:       "image-proof",
		Provider:   "aws",
		TargetOS:   targetLinux,
		ServerType: "m7i.large",
		CloudID:    "i-image",
		ServerName: "crabbox-image-proof",
		Host:       "203.0.113.10",
		SSHUser:    "crabbox",
		SSHPort:    "2222",
		WorkRoot:   defaultPOSIXWorkRoot,
		State:      "active",
		Image: &CoordinatorLeaseImage{
			ID:         "ami-devtools",
			Source:     "promoted",
			Provider:   "aws",
			Kind:       "aws-ami",
			Region:     "eu-west-1",
			PromotedAt: "2026-07-31T00:00:00Z",
		},
		ProvisioningTiming: &CoordinatorProvisioningTiming{
			RequestMs:      1200,
			NetworkReadyMs: 3400,
			BootstrapMs:    500,
			TotalMs:        5100,
			Phases: []CoordinatorProvisioningPhase{
				{Name: "request", Ms: 1200},
				{Name: "network_ready", Ms: 3400},
				{Name: "bootstrap", Ms: 500},
			},
		},
	}
	var stderr bytes.Buffer
	writeCoordinatorLeaseProvisioningDetails(&stderr, lease)
	for _, want := range []string{
		"image selected id=ami-devtools source=promoted kind=aws-ami region=eu-west-1 promoted_at=2026-07-31T00:00:00Z",
		"provider startup request=1.2s network_ready=3.4s bootstrap=500ms total=5.1s",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr=%q missing %q", stderr.String(), want)
		}
	}
	runnerTiming := coordinatorRunnerTiming(lease)
	if runnerTiming == nil || runnerTiming.TotalMs != 5100 || len(runnerTiming.Phases) != 3 {
		t.Fatalf("runner timing=%#v", runnerTiming)
	}
	var total int64
	for _, phase := range runnerTiming.Phases {
		total += phase.Ms
	}
	if total != runnerTiming.TotalMs {
		t.Fatalf("phase total=%d want %d: %#v", total, runnerTiming.TotalMs, runnerTiming.Phases)
	}
}

func TestCoordinatorRunnerTimingRejectsMalformedPhaseVectors(t *testing.T) {
	validLegacy := func() *CoordinatorProvisioningTiming {
		return &CoordinatorProvisioningTiming{RequestMs: 2, TotalMs: 10}
	}
	tests := []struct {
		name   string
		timing *CoordinatorProvisioningTiming
	}{
		{
			name: "unknown",
			timing: &CoordinatorProvisioningTiming{
				RequestMs: 2, TotalMs: 10,
				Phases: []CoordinatorProvisioningPhase{{Name: "dns", Ms: 10}},
			},
		},
		{
			name: "duplicate",
			timing: &CoordinatorProvisioningTiming{
				RequestMs: 2, TotalMs: 10,
				Phases: []CoordinatorProvisioningPhase{
					{Name: "request", Ms: 4},
					{Name: "request", Ms: 6},
				},
			},
		},
		{
			name: "nonpositive",
			timing: &CoordinatorProvisioningTiming{
				RequestMs: 2, TotalMs: 10,
				Phases: []CoordinatorProvisioningPhase{{Name: "request", Ms: 0}},
			},
		},
		{
			name: "over budget",
			timing: &CoordinatorProvisioningTiming{
				RequestMs: 2, TotalMs: 10,
				Phases: []CoordinatorProvisioningPhase{{Name: "request", Ms: 11}},
			},
		},
		{
			name: "over four",
			timing: &CoordinatorProvisioningTiming{
				RequestMs: 2, TotalMs: 10,
				Phases: []CoordinatorProvisioningPhase{
					{Name: "request", Ms: 1},
					{Name: "network_ready", Ms: 1},
					{Name: "bootstrap", Ms: 1},
					{Name: "unattributed", Ms: 1},
					{Name: "request", Ms: 1},
				},
			},
		},
		{
			name: "overflow",
			timing: &CoordinatorProvisioningTiming{
				TotalMs: math.MaxInt64,
				Phases: []CoordinatorProvisioningPhase{
					{Name: "request", Ms: math.MaxInt64},
					{Name: "network_ready", Ms: 1},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runnerTiming := coordinatorRunnerTiming(CoordinatorLease{ProvisioningTiming: test.timing})
			if runnerTiming == nil {
				t.Fatal("runner timing is nil")
			}
			if test.name == "overflow" {
				if len(runnerTiming.Phases) != 1 || runnerTiming.Phases[0] != (RunnerPhase{Name: "provider.unattributed", Ms: math.MaxInt64}) {
					t.Fatalf("overflow fallback=%#v", runnerTiming.Phases)
				}
				return
			}
			want := coordinatorRunnerTiming(CoordinatorLease{ProvisioningTiming: validLegacy()})
			if !reflect.DeepEqual(runnerTiming, want) {
				t.Fatalf("runner timing=%#v want legacy fallback %#v", runnerTiming, want)
			}
		})
	}
}

func TestCoordinatorRunnerTimingFallsBackWithoutFailingDecode(t *testing.T) {
	want := []RunnerPhase{
		{Name: "provider.request", Ms: 2},
		{Name: "connect.provider", Ms: 3},
		{Name: "provider.unattributed", Ms: 5},
	}
	for _, phases := range []string{
		`"invalid"`,
		`[{"name":3,"ms":1}]`,
		`[{"name":"request","ms":"1"}]`,
		`[{"name":"request","ms":1.5}]`,
		`[{"name":"request","ms":1e3}]`,
		`[{"name":"request","ms":null}]`,
		`[{"name":"request"}]`,
		`[{"name":"request","ms":0}]`,
		`[{"name":"request","ms":-1}]`,
		`[{"name":"request","ms":9223372036854775808}]`,
	} {
		t.Run(phases, func(t *testing.T) {
			var lease CoordinatorLease
			err := json.Unmarshal([]byte(fmt.Sprintf(`{
				"id":"cbx_123",
				"provisioningTiming":{
					"requestMs":2,
					"networkReadyMs":3,
					"totalMs":10,
					"phases":%s
				}
			}`, phases)), &lease)
			if err != nil {
				t.Fatalf("optional timing failed lease decode: %v", err)
			}
			runnerTiming := coordinatorRunnerTiming(lease)
			if runnerTiming == nil || !reflect.DeepEqual(runnerTiming.Phases, want) {
				t.Fatalf("runner timing=%#v want %#v", runnerTiming, want)
			}
		})
	}
}

func TestCoordinatorProvisioningTimingRejectsMalformedScalars(t *testing.T) {
	for _, test := range []struct {
		name string
		json string
	}{
		{name: "request", json: `{"requestMs":"invalid","totalMs":10}`},
		{name: "network ready", json: `{"networkReadyMs":1.5,"totalMs":10}`},
		{name: "bootstrap", json: `{"bootstrapMs":{},"totalMs":10}`},
		{name: "total", json: `{"totalMs":[]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var timing CoordinatorProvisioningTiming
			if err := json.Unmarshal([]byte(test.json), &timing); err == nil {
				t.Fatalf("json.Unmarshal(%s) succeeded, want malformed scalar error", test.json)
			}
		})
	}
}

func TestCoordinatorProvisioningTimingRejectsNonObjects(t *testing.T) {
	for _, input := range []string{`[]`, `"invalid"`, `42`, `true`} {
		t.Run(input, func(t *testing.T) {
			var timing CoordinatorProvisioningTiming
			if err := json.Unmarshal([]byte(input), &timing); err == nil {
				t.Fatalf("json.Unmarshal(%s) succeeded, want non-object error", input)
			}
		})
	}
}

func TestCoordinatorAcquireProvisioningTimingDecode(t *testing.T) {
	for _, test := range []struct {
		name       string
		timingJSON string
		wantErr    bool
	}{
		{
			name:       "decimal phase preserves scalar fallback",
			timingJSON: `{"requestMs":2,"networkReadyMs":3,"totalMs":10,"phases":[{"name":"request","ms":1.5}]}`,
		},
		{
			name:       "quoted phase preserves scalar fallback",
			timingJSON: `{"requestMs":2,"networkReadyMs":3,"totalMs":10,"phases":[{"name":"request","ms":"1"}]}`,
		},
		{
			name:       "exponent phase preserves scalar fallback",
			timingJSON: `{"requestMs":2,"networkReadyMs":3,"totalMs":10,"phases":[{"name":"request","ms":1e3}]}`,
		},
		{
			name:       "null phase preserves scalar fallback",
			timingJSON: `{"requestMs":2,"networkReadyMs":3,"totalMs":10,"phases":[{"name":"request","ms":null}]}`,
		},
		{
			name:       "missing phase preserves scalar fallback",
			timingJSON: `{"requestMs":2,"networkReadyMs":3,"totalMs":10,"phases":[{"name":"request"}]}`,
		},
		{
			name:       "zero phase preserves scalar fallback",
			timingJSON: `{"requestMs":2,"networkReadyMs":3,"totalMs":10,"phases":[{"name":"request","ms":0}]}`,
		},
		{
			name:       "negative phase preserves scalar fallback",
			timingJSON: `{"requestMs":2,"networkReadyMs":3,"totalMs":10,"phases":[{"name":"request","ms":-1}]}`,
		},
		{
			name:       "overflow phase preserves scalar fallback",
			timingJSON: `{"requestMs":2,"networkReadyMs":3,"totalMs":10,"phases":[{"name":"request","ms":9223372036854775808}]}`,
		},
		{
			name:       "malformed total is rejected",
			timingJSON: `{"requestMs":2,"totalMs":"invalid","phases":[{"name":"request","ms":2}]}`,
			wantErr:    true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/leases" {
					http.NotFound(w, r)
					return
				}
				fmt.Fprintf(w, `{"lease":{"id":"cbx_timing","provisioningTiming":%s}}`, test.timingJSON)
			}))
			defer server.Close()

			client := &CoordinatorClient{
				BaseURL: server.URL,
				Token:   "user-token",
				Client:  server.Client(),
			}
			cfg := baseConfig()
			cfg.Provider = "aws"
			lease, err := client.CreateLease(context.Background(), cfg, "ssh-ed25519 test", false, "cbx_timing", "timing")
			if test.wantErr {
				if err == nil {
					t.Fatal("CreateLease succeeded, want malformed total error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			runnerTiming := coordinatorRunnerTiming(lease)
			want := []RunnerPhase{
				{Name: "provider.request", Ms: 2},
				{Name: "connect.provider", Ms: 3},
				{Name: "provider.unattributed", Ms: 5},
			}
			if runnerTiming == nil || !reflect.DeepEqual(runnerTiming.Phases, want) {
				t.Fatalf("runner timing=%#v want %#v", runnerTiming, want)
			}
		})
	}
}

func TestCoordinatorRunnerTimingKeepsAcceptedPhaseNamesUnique(t *testing.T) {
	runnerTiming := coordinatorRunnerTiming(CoordinatorLease{
		ProvisioningTiming: &CoordinatorProvisioningTiming{
			TotalMs: 10,
			Phases: []CoordinatorProvisioningPhase{
				{Name: "request", Ms: 1},
				{Name: "network_ready", Ms: 1},
				{Name: "bootstrap", Ms: 1},
				{Name: "unattributed", Ms: 1},
			},
		},
	})
	want := []RunnerPhase{
		{Name: "provider.request", Ms: 1},
		{Name: "connect.provider", Ms: 1},
		{Name: "bootstrap.readiness", Ms: 1},
		{Name: "provider.unattributed", Ms: 7},
	}
	if runnerTiming == nil || !reflect.DeepEqual(runnerTiming.Phases, want) {
		t.Fatalf("runner timing=%#v want %#v", runnerTiming, want)
	}
}

func TestCoordinatorRunnerTimingUsesUnattributedForInconsistentLegacyScalars(t *testing.T) {
	runnerTiming := coordinatorRunnerTiming(CoordinatorLease{
		ProvisioningTiming: &CoordinatorProvisioningTiming{
			RequestMs:      8,
			NetworkReadyMs: 3,
			TotalMs:        10,
		},
	})
	want := []RunnerPhase{{Name: "provider.unattributed", Ms: 10}}
	if runnerTiming == nil || !reflect.DeepEqual(runnerTiming.Phases, want) {
		t.Fatalf("runner timing=%#v want %#v", runnerTiming, want)
	}
}

func TestCoordinatorCreateLeaseTimesOutWithDiagnostics(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "test@example.com")
	oldTimeout := coordinatorCreateLeaseTimeoutForConfig
	oldProgressInterval := coordinatorCreateLeaseProgressInterval
	oldRecoveryTimeout := coordinatorCreateLeaseRecoveryTimeout
	oldRecoveryInterval := coordinatorCreateLeaseRecoveryInterval
	oldAttemptID := coordinatorCreateAttemptID
	coordinatorCreateLeaseTimeoutForConfig = func(Config) time.Duration { return 80 * time.Millisecond }
	coordinatorCreateLeaseProgressInterval = 10 * time.Millisecond
	coordinatorCreateLeaseRecoveryTimeout = 30 * time.Millisecond
	coordinatorCreateLeaseRecoveryInterval = 5 * time.Millisecond
	coordinatorCreateAttemptID = func() string { return "cat_99999999999999999999999999999999" }
	defer func() {
		coordinatorCreateLeaseTimeoutForConfig = oldTimeout
		coordinatorCreateLeaseProgressInterval = oldProgressInterval
		coordinatorCreateLeaseRecoveryTimeout = oldRecoveryTimeout
		coordinatorCreateLeaseRecoveryInterval = oldRecoveryInterval
		coordinatorCreateAttemptID = oldAttemptID
	}()

	var cancellations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
			time.Sleep(250 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: "cbx_timeout"}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/cbx_timeout/cancel-create":
			cancellations.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"canceledCreate": map[string]any{
				"version": 1, "requestedLeaseID": "cbx_timeout",
				"createAttemptID": "cat_99999999999999999999999999999999", "state": "canceled",
			}})
		default:
			http.NotFound(w, r)
		}
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
	if !strings.Contains(stderr.String(), "waiting for coordinator lease provider=azure slug=crimson-lobster") ||
		!strings.Contains(stderr.String(), "abandoning uncertain coordinator create cbx_timeout; recording durable cancellation") {
		t.Fatalf("missing progress or cancellation output: %q", stderr.String())
	}
	if got := cancellations.Load(); got != 1 {
		t.Fatalf("cancel-create requests=%d, want 1", got)
	}
}

func TestCoordinatorCreateLeaseCancellationUsesExactDurableAttemptToken(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "test@example.com")
	oldRecoveryTimeout := coordinatorCanceledCreateRecoveryTimeout
	oldRecoveryInterval := coordinatorCreateLeaseRecoveryInterval
	coordinatorCanceledCreateRecoveryTimeout = time.Second
	coordinatorCreateLeaseRecoveryInterval = 10 * time.Millisecond
	oldAttemptID := coordinatorCreateAttemptID
	coordinatorCreateAttemptID = func() string { return "cat_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }
	defer func() {
		coordinatorCanceledCreateRecoveryTimeout = oldRecoveryTimeout
		coordinatorCreateLeaseRecoveryInterval = oldRecoveryInterval
		coordinatorCreateAttemptID = oldAttemptID
	}()

	createStarted := make(chan struct{})
	allowCreateCommit := make(chan struct{})
	unblockCreate := sync.OnceFunc(func() { close(allowCreateCommit) })
	firstReconcileRelease := make(chan struct{})
	releaseObserved := make(chan struct{})
	var firstReleaseAttempt sync.Once
	var firstRelease sync.Once
	var releaseCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
			var body struct {
				CreateAttemptID string `json:"createAttemptID"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.CreateAttemptID != "cat_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
				t.Errorf("create attempt=%q", body.CreateAttemptID)
			}
			close(createStarted)
			<-allowCreateCommit // Model a coordinator/provider path that commits after client cancellation.
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID:       "cbx_cancel_late",
				Slug:     "pearl-prawn",
				Provider: "aws",
				TargetOS: targetLinux,
				Host:     "203.0.113.44",
				State:    "active",
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/cbx_cancel_late/cancel-create":
			var body struct {
				CreateAttemptID string `json:"createAttemptID"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.CreateAttemptID != "cat_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
				t.Errorf("cancel attempt=%q", body.CreateAttemptID)
			}
			firstReleaseAttempt.Do(func() { close(firstReconcileRelease) })
			releaseCount.Add(1)
			firstRelease.Do(func() { close(releaseObserved) })
			_ = json.NewEncoder(w).Encode(map[string]any{"canceledCreate": map[string]any{
				"version": 1, "requestedLeaseID": "cbx_cancel_late",
				"createAttemptID": body.CreateAttemptID, "state": "canceled",
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

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan coordinatorCreateLeaseResult, 1)
	createFinished := make(chan struct{})
	// Join before server.Close and the earlier global-restoration defer.
	defer func() {
		cancel()
		unblockCreate()
		<-createFinished
	}()
	go func() {
		defer close(createFinished)
		lease, err := backend.createCoordinatorLeaseWithProgress(
			ctx,
			cfg,
			"ssh-rsa test",
			false,
			"cbx_cancel_late",
			"pearl-prawn",
		)
		resultCh <- coordinatorCreateLeaseResult{lease: lease, err: err}
	}()
	select {
	case <-createStarted:
	case <-createFinished:
		t.Fatalf("create ended before its request started: %v", (<-resultCh).err)
	}
	cancel()

	select {
	case <-firstReconcileRelease:
	case <-createFinished:
		// Completion and receipt observation can become ready together.
		select {
		case <-firstReconcileRelease:
		default:
			t.Fatal("canceled create did not send its pre-generated create attempt token")
		}
	}
	unblockCreate()

	select {
	case result := <-resultCh:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("create err=%v, want context canceled", result.err)
		}
		if result.lease.ID != "" {
			t.Fatalf("canceled create adopted lease=%#v", result.lease)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled create did not return within its reconciliation bound")
	}

	select {
	case <-releaseObserved:
	case <-time.After(time.Second):
		t.Fatalf("durable create cancellation was not recorded; stderr=%q", stderr.String())
	}
	if got := releaseCount.Load(); got != 1 {
		t.Fatalf("cancel-create requests=%d, want 1", got)
	}
}

func TestCanceledCoordinatorCreateRetriesTransientCancelFailure(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "test@example.com")
	oldRecoveryInterval := coordinatorCreateLeaseRecoveryInterval
	coordinatorCreateLeaseRecoveryInterval = time.Millisecond
	defer func() { coordinatorCreateLeaseRecoveryInterval = oldRecoveryInterval }()

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/cbx_cancel_transient/cancel-create" {
			http.NotFound(w, r)
			return
		}
		if attempts.Add(1) == 1 {
			http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"canceledCreate": map[string]any{
			"version": 1, "requestedLeaseID": "cbx_cancel_transient",
			"createAttemptID": "cat_11111111111111111111111111111111", "state": "canceled",
		}})
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
	backend := &coordinatorLeaseBackend{coord: coord, rt: Runtime{Stderr: &bytes.Buffer{}}}
	recoverCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := backend.cancelCoordinatorLeaseCreate(recoverCtx, "cbx_cancel_transient", "retry-crab", "cat_11111111111111111111111111111111"); err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("cancel-create attempts=%d, want transient failure plus retry", got)
	}
}

func TestCoordinatorCancelCreateRetryableStatuses(t *testing.T) {
	for _, statusCode := range []int{
		http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
	} {
		err := CoordinatorHTTPError{StatusCode: statusCode}
		if !coordinatorCancelCreateErrorRetryable(err) {
			t.Fatalf("status %d must be retryable", statusCode)
		}
	}
	if coordinatorCancelCreateErrorRetryable(CoordinatorHTTPError{StatusCode: http.StatusConflict}) {
		t.Fatal("conflict must not be retried")
	}
}

func TestCoordinatorCreateLeaseTreatsAllServerErrorsAsAmbiguous(t *testing.T) {
	for _, statusCode := range []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		if !coordinatorCreateLeaseErrorMayHaveCommitted(CoordinatorHTTPError{StatusCode: statusCode}) {
			t.Fatalf("status %d must be treated as an ambiguous create result", statusCode)
		}
	}
	if coordinatorCreateLeaseErrorMayHaveCommitted(CoordinatorHTTPError{StatusCode: http.StatusConflict}) {
		t.Fatal("conflict must remain definitive")
	}
}

func TestCanceledCoordinatorCreateMakesOneFreshFinalCancelAttemptAtRecoveryDeadline(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "test@example.com")
	// Advance the recovery deadline only once the mock request is blocked.
	synctest.Test(t, func(t *testing.T) {
		oldFinalTimeout := coordinatorCanceledCreateFinalTimeout
		oldRecoveryInterval := coordinatorCreateLeaseRecoveryInterval
		coordinatorCanceledCreateFinalTimeout = time.Second
		coordinatorCreateLeaseRecoveryInterval = time.Hour
		defer func() {
			coordinatorCanceledCreateFinalTimeout = oldFinalTimeout
			coordinatorCreateLeaseRecoveryInterval = oldRecoveryInterval
		}()

		var attempts atomic.Int32
		var deadlines [2]time.Time
		coord := &CoordinatorClient{
			BaseURL: "http://coordinator.test",
			Token:   "user-token",
			Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				attempt := int(attempts.Add(1))
				if req.Method != http.MethodPost || req.URL.Path != "/v1/leases/cbx_cancel_final/cancel-create" {
					return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
				}
				deadline, ok := req.Context().Deadline()
				if !ok {
					return nil, errors.New("cancel-create attempt had no deadline")
				}
				if attempt > len(deadlines) {
					return nil, fmt.Errorf("unexpected cancel-create attempt %d", attempt)
				}
				deadlines[attempt-1] = deadline
				if attempt == 1 {
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				if err := req.Context().Err(); err != nil {
					return nil, fmt.Errorf("final cancel-create started with expired context: %w", err)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(
						`{"canceledCreate":{"version":1,"requestedLeaseID":"cbx_cancel_final","createAttemptID":"cat_22222222222222222222222222222222","state":"canceled"}}`,
					)),
					Request: req,
				}, nil
			})},
		}
		backend := &coordinatorLeaseBackend{coord: coord, rt: Runtime{Stderr: &bytes.Buffer{}}}
		recoverCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		if err := backend.cancelCoordinatorLeaseCreate(recoverCtx, "cbx_cancel_final", "final-crab", "cat_22222222222222222222222222222222"); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(recoverCtx.Err(), context.DeadlineExceeded) {
			t.Fatalf("recovery err=%v, want deadline exceeded", recoverCtx.Err())
		}
		if got := attempts.Load(); got != 2 {
			t.Fatalf("cancel-create attempts=%d, want recovery attempt plus exactly one final attempt", got)
		}
		if !deadlines[1].After(deadlines[0]) {
			t.Fatalf("final deadline=%s, want fresh deadline after expired recovery deadline=%s", deadlines[1], deadlines[0])
		}
	})
}

func TestCoordinatorFixedCreateCancellationDoesNotReleaseDurableLease(t *testing.T) {
	putStarted := make(chan struct{})
	allowPutReturn := make(chan struct{})
	putFinished := make(chan struct{})
	var cleanupRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/leases/cbx_fixed_cancel":
			close(putStarted)
			<-allowPutReturn
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: "cbx_fixed_cancel", Slug: "durable-crab", Provider: "aws", TargetOS: targetLinux,
				State: "active", Host: "203.0.113.55",
			}})
			close(putFinished)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/leases/cbx_fixed_cancel/"):
			cleanupRequests.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: "cbx_fixed_cancel", State: "released"}})
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
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &bytes.Buffer{}}}
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan coordinatorCreateLeaseResult, 1)
	go func() {
		lease, createErr := backend.createCoordinatorLeaseWithProgressMode(
			ctx, cfg, "ssh-ed25519 test", true, "cbx_fixed_cancel", "durable-crab", true,
		)
		resultCh <- coordinatorCreateLeaseResult{lease: lease, err: createErr}
	}()
	<-putStarted
	cancel()

	select {
	case result := <-resultCh:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("fixed create err=%v, want context canceled", result.err)
		}
		if result.lease.ID != "" {
			t.Fatalf("fixed canceled create returned lease=%#v", result.lease)
		}
	case <-time.After(time.Second):
		t.Fatal("fixed canceled create did not return")
	}
	close(allowPutReturn)
	select {
	case <-putFinished:
	case <-time.After(time.Second):
		t.Fatal("fixed PUT handler did not finish")
	}
	if got := cleanupRequests.Load(); got != 0 {
		t.Fatalf("fixed replay-owned lease received %d cancellation cleanup request(s)", got)
	}
}

func TestCanceledCoordinatorCreateJoinsDefinitiveCreateError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	createErr := CoordinatorHTTPError{
		Method:     http.MethodPost,
		Path:       "/v1/leases",
		StatusCode: http.StatusBadRequest,
		Message:    `{"error":"invalid_configuration"}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"canceledCreate": map[string]any{
			"version": 1, "requestedLeaseID": "cbx_definitive_cancel",
			"createAttemptID": "cat_33333333333333333333333333333333", "state": "canceled",
		}})
	}))
	defer server.Close()
	backend := &coordinatorLeaseBackend{
		coord: &CoordinatorClient{BaseURL: server.URL, Client: server.Client()},
		rt:    Runtime{Stderr: &bytes.Buffer{}},
	}

	err := backend.canceledCoordinatorLeaseCreateError(ctx, "cbx_definitive_cancel", "amber-crab", "cat_33333333333333333333333333333333", false, createErr)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want caller cancellation", err)
	}
	var gotHTTPError CoordinatorHTTPError
	if !errors.As(err, &gotHTTPError) || gotHTTPError.StatusCode != http.StatusBadRequest {
		t.Fatalf("err=%v, want joined definitive create error", err)
	}
}

func TestCanceledCoordinatorCreateAcceptsDurableTombstoneWithoutLease(t *testing.T) {
	var gets atomic.Int32
	var releases atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/cbx_cancel_retained/cancel-create":
			var body struct {
				CreateAttemptID string `json:"createAttemptID"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.CreateAttemptID != "cat_44444444444444444444444444444444" {
				t.Errorf("create attempt=%q", body.CreateAttemptID)
			}
			releases.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"canceledCreate": map[string]any{
				"version": 1, "requestedLeaseID": "cbx_cancel_retained",
				"createAttemptID": body.CreateAttemptID, "state": "canceled",
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
	backend := &coordinatorLeaseBackend{
		cfg:   cfg,
		coord: coord,
		rt:    Runtime{Stderr: &bytes.Buffer{}},
	}

	if err := backend.cancelCoordinatorLeaseCreate(
		context.Background(),
		"cbx_cancel_retained",
		"coral-crab",
		"cat_44444444444444444444444444444444",
	); err != nil {
		t.Fatal(err)
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("cancel-create requests=%d, want 1", got)
	}
	if got := gets.Load(); got != 0 {
		t.Fatalf("lease GET requests=%d, want none", got)
	}
}

func TestCanceledCoordinatorCreateValidatesAttestation(t *testing.T) {
	tests := []struct {
		name           string
		requestLeaseID any
		createAttempt  any
		wantError      bool
	}{
		{name: "correct", requestLeaseID: "cbx_cancel_expected", createAttempt: "cat_55555555555555555555555555555555"},
		{name: "incorrect lease", requestLeaseID: "cbx_cancel_other", createAttempt: "cat_55555555555555555555555555555555", wantError: true},
		{name: "incorrect token", requestLeaseID: "cbx_cancel_expected", createAttempt: "cat_66666666666666666666666666666666", wantError: true},
		{name: "missing", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/cbx_cancel_expected/cancel-create" {
					http.NotFound(w, r)
					return
				}
				attestation := map[string]any{
					"version": 1, "state": "canceled",
				}
				if tt.requestLeaseID != nil {
					attestation["requestedLeaseID"] = tt.requestLeaseID
				}
				if tt.createAttempt != nil {
					attestation["createAttemptID"] = tt.createAttempt
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"canceledCreate": attestation})
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
			backend := &coordinatorLeaseBackend{coord: coord, rt: Runtime{Stderr: &bytes.Buffer{}}}

			err = backend.cancelCoordinatorLeaseCreate(
				context.Background(),
				"cbx_cancel_expected",
				"expected-crab",
				"cat_55555555555555555555555555555555",
			)
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "mismatched cancel-create attestation") {
					t.Fatalf("err=%v, want missing or mismatched attestation", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCoordinatorCreateLeaseRecoversWithSameTokenBoundPost(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "test@example.com")
	oldRecoveryTimeout := coordinatorCreateLeaseRecoveryTimeout
	oldRecoveryInterval := coordinatorCreateLeaseRecoveryInterval
	coordinatorCreateLeaseRecoveryTimeout = time.Second
	coordinatorCreateLeaseRecoveryInterval = 10 * time.Millisecond
	defer func() {
		coordinatorCreateLeaseRecoveryTimeout = oldRecoveryTimeout
		coordinatorCreateLeaseRecoveryInterval = oldRecoveryInterval
	}()

	var createdLeaseID string
	const canonicalLeaseID = "cbx_recovered_canonical"
	var createAttemptID string
	posts := 0
	gets := 0
	releases := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
			posts++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			createdLeaseID, _ = body["leaseID"].(string)
			gotAttemptID, _ := body["createAttemptID"].(string)
			if createAttemptID == "" {
				createAttemptID = gotAttemptID
			}
			if gotAttemptID != createAttemptID {
				t.Fatalf("create attempt changed across retry: first=%q got=%q", createAttemptID, gotAttemptID)
			}
			if posts == 1 {
				http.Error(w, "error code: 1101", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: canonicalLeaseID, Slug: "jade-crab", Provider: "azure", TargetOS: targetWindows,
				WindowsMode: windowsModeNormal, CloudID: "crabbox-jade-crab", Host: "203.0.113.44",
				SSHUser: "crabbox", SSHPort: "22", WorkRoot: defaultWindowsWorkRoot,
				State: "active", ServerType: "Standard_D2ads_v6", IdleTimeoutSeconds: 1800,
			}})
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
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/release"):
			releases++
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID:       createdLeaseID,
				Provider: "azure",
				State:    "released",
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
	if lease.ID != canonicalLeaseID || lease.Host != "203.0.113.44" {
		t.Fatalf("lease=%#v created=%s", lease, createdLeaseID)
	}
	if posts != 2 || gets != 0 {
		t.Fatalf("posts=%d gets=%d, want same-token POST retry and no polling GET", posts, gets)
	}
	if releases != 0 {
		t.Fatalf("active uncertain create release requests=%d, want 0", releases)
	}
	for _, want := range []string{
		"uncertain result",
		"token-bound POST",
		createdLeaseID,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr=%q missing %q", stderr.String(), want)
		}
	}
}

func TestCoordinatorCreateLeaseDefinitiveErrorDoesNotReconcile(t *testing.T) {
	var gets atomic.Int32
	var releases atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
			http.Error(w, `{"error":"invalid_configuration"}`, http.StatusBadRequest)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/cbx_definitive":
			gets.Add(1)
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/cbx_definitive/release":
			releases.Add(1)
			http.NotFound(w, r)
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
	backend := &coordinatorLeaseBackend{
		cfg:   cfg,
		coord: coord,
		rt:    Runtime{Stderr: &bytes.Buffer{}},
	}

	lease, err := backend.createCoordinatorLeaseWithProgress(
		context.Background(),
		cfg,
		"ssh-rsa test",
		false,
		"cbx_definitive",
		"amber-crab",
	)
	if err == nil || !strings.Contains(err.Error(), "http 400") {
		t.Fatalf("create err=%v, want definitive http 400", err)
	}
	if lease.ID != "" {
		t.Fatalf("definitive error returned lease=%#v", lease)
	}
	if gets.Load() != 0 || releases.Load() != 0 {
		t.Fatalf("definitive error reconciliation gets=%d releases=%d, want 0/0", gets.Load(), releases.Load())
	}
}

func TestCoordinatorFixedCreateAmbiguousErrorRepeatsPutAndDoesNotAdoptConflictingGet(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "test@example.com")
	oldRecoveryTimeout := coordinatorCreateLeaseRecoveryTimeout
	oldRecoveryInterval := coordinatorCreateLeaseRecoveryInterval
	coordinatorCreateLeaseRecoveryTimeout = time.Second
	coordinatorCreateLeaseRecoveryInterval = time.Millisecond
	defer func() {
		coordinatorCreateLeaseRecoveryTimeout = oldRecoveryTimeout
		coordinatorCreateLeaseRecoveryInterval = oldRecoveryInterval
	}()

	puts := 0
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/leases/cbx_abcdef123462":
			puts++
			if puts == 1 {
				http.Error(w, "error code: 1101", http.StatusInternalServerError)
				return
			}
			http.Error(w, `{"error":"lease_id_conflict","message":"lease id is bound to another create intent"}`, http.StatusConflict)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/cbx_abcdef123462":
			gets++
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: "cbx_abcdef123462", Slug: "other-intent", Provider: "aws", TargetOS: targetLinux,
				State: "active", CloudID: "i-other", Host: "203.0.113.99",
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
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &bytes.Buffer{}}}
	_, err = backend.createCoordinatorLeaseWithProgressMode(
		context.Background(), cfg, "ssh-ed25519 test", true,
		"cbx_abcdef123462", "fixed-conflict", true,
	)
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("err=%v, want fixed intent conflict", err)
	}
	if puts != 2 || gets != 0 {
		t.Fatalf("puts=%d gets=%d, want exact PUT retry and no GET adoption", puts, gets)
	}
}

func TestCoordinatorRecoveredProvisioningKeepsCreationLifetime(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "test@example.com")
	for _, fixed := range []bool{true, false} {
		for _, test := range []struct {
			name          string
			createDelay   time.Duration
			readyAfter    time.Duration
			cancelAfter   time.Duration
			callerTimeout time.Duration
			wantElapsed   time.Duration
			wantErr       error
		}{
			{name: "activation beyond recovery window", createDelay: 2 * time.Minute, readyAfter: 4 * time.Minute, wantElapsed: 4 * time.Minute},
			{name: "original creation deadline", createDelay: 29 * time.Minute, readyAfter: 31 * time.Minute, wantElapsed: 30 * time.Minute, wantErr: context.DeadlineExceeded},
			{name: "caller cancellation", createDelay: 2 * time.Minute, readyAfter: 4 * time.Minute, cancelAfter: 3 * time.Minute, wantElapsed: 3 * time.Minute, wantErr: context.Canceled},
			{name: "earlier caller deadline", createDelay: 2 * time.Minute, readyAfter: 4 * time.Minute, callerTimeout: 3 * time.Minute, wantElapsed: 3 * time.Minute, wantErr: context.DeadlineExceeded},
		} {
			t.Run(fmt.Sprintf("fixed=%v/%s", fixed, test.name), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					const requestedID = "cbx_abcdef123463"
					canonicalID := "cbx_abcdef123464"
					target := targetMacOS
					createMethod, createPath := http.MethodPost, "/v1/leases"
					if fixed {
						canonicalID = requestedID
						target = targetLinux
						createMethod, createPath = http.MethodPut, createPath+"/"+requestedID
					}
					started := time.Now()
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					if test.cancelAfter > 0 {
						cancelTimer := time.AfterFunc(test.cancelAfter, cancel)
						defer cancelTimer.Stop()
					}
					if test.callerTimeout > 0 {
						var cancelDeadline context.CancelFunc
						ctx, cancelDeadline = context.WithTimeout(ctx, test.callerTimeout)
						defer cancelDeadline()
					}
					respond := func(code int, body any) (*http.Response, error) {
						data, err := json.Marshal(body)
						if err != nil {
							return nil, err
						}
						return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
					}
					var initialRequest map[string]any
					var attemptID string
					creates, gets, cancellations := 0, 0, 0
					coord := &CoordinatorClient{
						BaseURL: "http://coordinator.test",
						Token:   "user-token",
						Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
							lease := CoordinatorLease{ID: canonicalID, Slug: "recovered-crab", Provider: "aws", TargetOS: target, State: "provisioning"}
							switch {
							case req.Method == createMethod && req.URL.Path == createPath:
								creates++
								var body map[string]any
								if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
									return nil, err
								}
								if creates == 1 {
									initialRequest = body
									attemptID, _ = body["createAttemptID"].(string)
									if body["leaseID"] != requestedID || (attemptID == "") != fixed {
										t.Errorf("unexpected create identity: lease=%v attempt=%q fixed=%v", body["leaseID"], attemptID, fixed)
									}
									time.Sleep(test.createDelay)
									return respond(http.StatusInternalServerError, map[string]string{"error": "response_lost"})
								}
								if creates != 2 || !reflect.DeepEqual(body, initialRequest) {
									t.Errorf("recovery changed create intent or created again: requests=%d", creates)
								}
								return respond(http.StatusAccepted, map[string]any{"lease": lease})
							case req.Method == http.MethodGet && req.URL.Path == "/v1/leases/"+canonicalID:
								gets++
								if creates != 2 {
									t.Error("polled before confirming the create intent")
								}
								if time.Since(started) >= test.readyAfter {
									lease.State, lease.Host = "active", "203.0.113.44"
								}
								return respond(http.StatusOK, map[string]any{"lease": lease})
							case req.Method == http.MethodPost && req.URL.Path == "/v1/leases/"+requestedID+"/cancel-create":
								cancellations++
								var body struct {
									CreateAttemptID string `json:"createAttemptID"`
								}
								if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
									return nil, err
								}
								if fixed || body.CreateAttemptID == "" || body.CreateAttemptID != attemptID {
									t.Error("cancellation lost the original create ownership")
								}
								return respond(http.StatusOK, map[string]any{"canceledCreate": map[string]any{
									"version": 1, "requestedLeaseID": requestedID, "createAttemptID": body.CreateAttemptID, "state": "canceled",
								}})
							default:
								t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
								return respond(http.StatusBadRequest, map[string]string{"error": "unexpected_request"})
							}
						})},
					}
					cfg := baseConfig()
					cfg.Provider, cfg.TargetOS = "aws", target
					backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: &bytes.Buffer{}}}
					lease, err := backend.createCoordinatorLeaseWithProgressMode(ctx, cfg, "ssh-ed25519 test", true, requestedID, "recovered-crab", fixed)
					if !errors.Is(err, test.wantErr) {
						t.Errorf("create error=%v, want %v", err, test.wantErr)
					}
					if test.wantErr == nil {
						if lease.ID != canonicalID || lease.State != "active" || lease.Host == "" {
							t.Errorf("create did not return the ready canonical lease: %#v", lease)
						}
					} else if lease.ID != "" {
						t.Errorf("failed create adopted lease: %#v", lease)
					}
					wantCancellations := 0
					if !fixed && test.wantErr != nil {
						wantCancellations = 1
					}
					if creates != 2 || gets == 0 || cancellations != wantCancellations {
						t.Errorf("create/get/cancel requests=%d/%d/%d, want 2/positive/%d", creates, gets, cancellations, wantCancellations)
					}
					if elapsed := time.Since(started); elapsed != test.wantElapsed {
						t.Errorf("create elapsed=%v, want original lifetime %v", elapsed, test.wantElapsed)
					}
				})
			})
		}
	}
}

func TestLeaseToServerTargetProjectsCanonicalLeaseLabels(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetLinux

	server, _, _ := leaseToServerTarget(CoordinatorLease{
		ID:                 "cbx_123",
		Slug:               "blue-lobster",
		Pond:               "evaluation",
		ServerType:         "c7a.large",
		Market:             "on-demand",
		Keep:               true,
		ExpiresAt:          "2026-08-23T07:00:00Z",
		LastTouchedAt:      "2026-08-23T06:30:00Z",
		IdleTimeoutSeconds: 1800,
		Desktop:            true,
		Browser:            true,
		Code:               true,
	}, cfg)

	for key, want := range map[string]string{
		"lease":             "cbx_123",
		"slug":              "blue-lobster",
		"keep":              "true",
		"target":            targetLinux,
		"pond":              "evaluation",
		"provider":          "aws",
		"server_type":       "c7a.large",
		"market":            "on-demand",
		"expires_at":        "2026-08-23T07:00:00Z",
		"last_touched_at":   "2026-08-23T06:30:00Z",
		"idle_timeout_secs": "1800",
		"desktop":           "true",
		"browser":           "true",
		"code":              "true",
	} {
		if server.Labels[key] != want {
			t.Fatalf("labels[%q]=%q, want %q; labels=%#v", key, server.Labels[key], want, server.Labels)
		}
	}
	if server.Provider != "aws" {
		t.Fatalf("provider=%q, want resolved fallback aws", server.Provider)
	}

	withoutMarket, _, _ := leaseToServerTarget(CoordinatorLease{ID: "cbx_456"}, cfg)
	if _, ok := withoutMarket.Labels["market"]; ok {
		t.Fatalf("market label=%q, want omitted", withoutMarket.Labels["market"])
	}
}

func TestLeaseToServerTargetAppliesAWSCloudInitReadiness(t *testing.T) {
	for _, test := range []struct {
		name          string
		provider      string
		leaseProvider string
		targetOS      string
		wantCloudInit bool
	}{
		{name: "AWS Linux", provider: "aws", leaseProvider: "aws", targetOS: targetLinux, wantCloudInit: true},
		{name: "authoritative AWS lease", provider: "hetzner", leaseProvider: "aws", targetOS: targetLinux, wantCloudInit: true},
		{name: "AWS Windows", provider: "aws", leaseProvider: "aws", targetOS: targetWindows},
		{name: "AWS macOS", provider: "aws", leaseProvider: "aws", targetOS: targetMacOS},
		{name: "other Linux provider", provider: "hetzner", leaseProvider: "hetzner", targetOS: targetLinux},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Provider = test.provider
			cfg.TargetOS = targetLinux
			_, target, _ := leaseToServerTarget(CoordinatorLease{
				ID: "cbx_123", Provider: test.leaseProvider, TargetOS: test.targetOS,
				Host: "203.0.113.10", SSHUser: "crabbox", SSHPort: "22",
			}, cfg)
			if test.wantCloudInit {
				want := "timeout 20m cloud-init status --wait >/tmp/crabbox-cloud-init.log 2>&1 && " + sshReadyCommand(SSHTarget{TargetOS: targetLinux})
				if target.ReadyCheck != want {
					t.Fatalf("coordinator AWS ready check=%q, want current-boot cloud-init followed by canonical readiness %q", target.ReadyCheck, want)
				}
			} else if target.ReadyCheck != "" {
				t.Fatalf("ready check=%q, want unchanged default", target.ReadyCheck)
			}
		})
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

func TestCoordinatorResolveDropsHistoricalProvisioningTiming(t *testing.T) {
	const leaseID = "cbx_historical_timing"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/leases/"+leaseID {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
			ID: leaseID, Slug: "reused-timing", Provider: "aws", TargetOS: targetLinux,
			CloudID: "i-reused", Host: "203.0.113.10", SSHUser: "crabbox", SSHPort: "22", State: "active",
			ProvisioningTiming: &CoordinatorProvisioningTiming{
				RequestMs:      2,
				NetworkReadyMs: 3,
				BootstrapMs:    4,
				TotalMs:        9,
			},
		}})
	}))
	defer server.Close()

	backend := newCoordinatorIdentityTestBackend(t, server.URL, "")
	resolved, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.runnerTiming != nil {
		t.Fatalf("resolved reused lease retained historical timing: %#v", resolved.runnerTiming)
	}
	if resolved.LeaseID != leaseID || resolved.Server.CloudID != "i-reused" {
		t.Fatalf("resolved lease identity=%#v", resolved)
	}
}

func TestCoordinatorProviderIdentityValidationCanonicalizesAliases(t *testing.T) {
	backend := &coordinatorLeaseBackend{
		spec: ProviderSpec{Name: "gcp"},
		cfg:  Config{Provider: "gcp"},
	}
	if err := backend.validateCoordinatorLeaseProviderIdentity(CoordinatorLease{
		ID:       "cbx_alias",
		Provider: " google-cloud ",
	}); err != nil {
		t.Fatalf("canonical alias rejected: %v", err)
	}
}

func TestCoordinatorCreateCanonicalProviderMatching(t *testing.T) {
	cfg := Config{Provider: "gcp", TargetOS: targetLinux}
	backend := &coordinatorLeaseBackend{cfg: cfg}
	base := CoordinatorLease{ID: "cbx_recovered", State: "active", Host: "203.0.113.10", TargetOS: targetLinux}
	for _, test := range []struct {
		name     string
		provider string
		want     bool
	}{
		{name: "canonical", provider: "gcp", want: true},
		{name: "alias", provider: "google-cloud", want: true},
		{name: "legacy empty", provider: "", want: true},
		{name: "cross provider", provider: "aws"},
		{name: "distinct azure route", provider: "azure-dynamic-sessions"},
		{name: "unknown", provider: "future-cloud"},
	} {
		t.Run(test.name, func(t *testing.T) {
			lease := base
			lease.Provider = test.provider
			if got := backend.validateCoordinatorLeaseCreateResult(cfg, lease, lease.ID, false) == nil; got != test.want {
				t.Fatalf("recovered=%t want %t for provider=%q", got, test.want, test.provider)
			}
		})
	}
}

func TestCoordinatorListCanonicalProviderMatching(t *testing.T) {
	leases := []CoordinatorLease{
		{ID: "canonical", Provider: "gcp"},
		{ID: "alias", Provider: "google-cloud"},
		{ID: "cross", Provider: "aws"},
		{ID: "empty"},
		{ID: "unknown", Provider: "future-cloud"},
	}
	filtered := filterCoordinatorLeasesForProvider(leases, "gcp")
	if len(filtered) != 2 || filtered[0].ID != "canonical" || filtered[1].ID != "alias" {
		t.Fatalf("filtered leases=%#v", filtered)
	}
}

func TestCoordinatorResolveEnforcesSelectedProviderIdentity(t *testing.T) {
	for _, test := range []struct {
		name             string
		returnedProvider string
		wantErr          bool
	}{
		{name: "same provider with changed capacity", returnedProvider: "aws"},
		{name: "legacy omission", returnedProvider: ""},
		{name: "different provider", returnedProvider: "external", wantErr: true},
		{name: "unknown provider", returnedProvider: "future-cloud", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/leases/cbx_identity" {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
					ID:                  "cbx_identity",
					Provider:            test.returnedProvider,
					TargetOS:            targetLinux,
					ServerType:          "c7a.48xlarge",
					RequestedServerType: "t3.small",
					Market:              "on-demand",
					CapacityHints: []CapacityHint{{
						Code: "fallback", ServerType: "c7a.48xlarge",
					}},
					CloudID: "i-identity",
					Host:    "203.0.113.10",
					State:   "active",
				}})
			}))
			defer server.Close()

			backend := newCoordinatorIdentityTestBackend(t, server.URL, "")
			lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "cbx_identity"})
			if test.wantErr {
				assertCoordinatorProviderIdentityError(t, err, test.returnedProvider, "cbx_identity")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if lease.Server.Provider != "aws" {
				t.Fatalf("provider=%q want selected aws", lease.Server.Provider)
			}
			if lease.Server.ServerType.Name != "c7a.48xlarge" {
				t.Fatalf("server type=%q, want coordinator capacity fallback", lease.Server.ServerType.Name)
			}
		})
	}
}

func TestCoordinatorAdminResolveEnforcesSelectedProviderIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/leases/cbx_admin_identity" {
			http.NotFound(w, r)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer user-token":
			http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
		case "Bearer admin-token":
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: "cbx_admin_identity", Provider: "external", State: "active",
			}})
		default:
			t.Fatalf("unexpected auth %q", r.Header.Get("Authorization"))
		}
	}))
	defer server.Close()

	backend := newCoordinatorIdentityTestBackend(t, server.URL, "admin-token")
	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: "cbx_admin_identity"})
	assertCoordinatorProviderIdentityError(t, err, "external", "cbx_admin_identity")
}

func TestCoordinatorStatusEnforcesSelectedProviderIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
			ID: "cbx_status_identity", Provider: "external", State: "active",
		}})
	}))
	defer server.Close()

	backend := newCoordinatorIdentityTestBackend(t, server.URL, "")
	_, err := backend.Status(context.Background(), StatusRequest{ID: "cbx_status_identity"})
	assertCoordinatorProviderIdentityError(t, err, "external", "cbx_status_identity")
}

func TestCoordinatorTouchEnforcesInputAndResponseProviderIdentity(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
			ID: "cbx_touch_identity", Provider: "external", State: "active",
		}})
	}))
	defer server.Close()

	backend := newCoordinatorIdentityTestBackend(t, server.URL, "")
	input := LeaseTarget{LeaseID: "cbx_touch_identity", Server: Server{Provider: "external"}}
	if _, err := backend.Touch(context.Background(), TouchRequest{Lease: input}); err == nil {
		t.Fatal("touch accepted mismatching input provider")
	} else {
		assertCoordinatorProviderIdentityError(t, err, "external", "cbx_touch_identity")
	}
	if requests != 0 {
		t.Fatalf("input mismatch sent %d coordinator request(s), want zero", requests)
	}

	input.Server.Provider = "aws"
	if _, err := backend.Touch(context.Background(), TouchRequest{Lease: input}); err == nil {
		t.Fatal("touch accepted mismatching response provider")
	} else {
		assertCoordinatorProviderIdentityError(t, err, "external", "cbx_touch_identity")
	}
	if requests != 1 {
		t.Fatalf("response mismatch sent %d coordinator request(s), want one", requests)
	}
}

func TestCoordinatorReleaseRejectsInputProviderMismatchBeforeRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()

	backend := newCoordinatorIdentityTestBackend(t, server.URL, "admin-token")
	for _, provider := range []string{"", "external"} {
		err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{
			LeaseID: "cbx_release_identity",
			Server:  Server{Provider: provider},
		}})
		assertCoordinatorProviderIdentityError(t, err, provider, "cbx_release_identity")
	}
	if requests != 0 {
		t.Fatalf("release mismatch sent %d coordinator request(s), want zero", requests)
	}
}

func TestStopCoordinatorProviderMismatchDoesNotRelease(t *testing.T) {
	clearConfigEnv(t)
	isolateTestUserDirs(t)
	releases := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/cbx_stop_identity":
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: "cbx_stop_identity", Provider: "external", State: "active",
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/cbx_stop_identity/release":
			releases++
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: "cbx_stop_identity", State: "released"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "user-token")

	err := (App{Stdout: io.Discard, Stderr: io.Discard}).stop(context.Background(), []string{
		"--provider", "aws", "--id", "cbx_stop_identity",
	})
	assertCoordinatorProviderIdentityError(t, err, "external", "cbx_stop_identity")
	if releases != 0 {
		t.Fatalf("release requests=%d want zero", releases)
	}
}

func TestStopCoordinatorInspectFailureKeepsProviderBinding(t *testing.T) {
	clearConfigEnv(t)
	isolateTestUserDirs(t)
	var releaseBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/cbx_stop_fallback":
			http.Error(w, `{"error":"inspect_failed"}`, http.StatusInternalServerError)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/cbx_stop_fallback/release":
			if err := json.NewDecoder(r.Body).Decode(&releaseBody); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: "cbx_stop_fallback", Provider: "aws", State: "released",
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "user-token")

	if err := (App{Stdout: io.Discard, Stderr: io.Discard}).stop(context.Background(), []string{
		"--provider", "aws", "--id", "cbx_stop_fallback",
	}); err != nil {
		t.Fatal(err)
	}
	if releaseBody["expectedProvider"] != "aws" {
		t.Fatalf("release body=%#v, want expectedProvider=aws", releaseBody)
	}
}

func TestStopForceCoordinatorRequiresLiveExactLease(t *testing.T) {
	tests := []struct {
		name        string
		id          string
		leaseStatus int
		provider    string
		wantErr     string
		wantRelease int
	}{
		{name: "reject slug", id: "friendly-slug", wantErr: "requires an exact coordinator lease id"},
		{name: "reject failed inspection", id: "cbx_abcdef123456", leaseStatus: http.StatusInternalServerError, wantErr: "inspect_failed"},
		{name: "reject wrong provider", id: "cbx_abcdef123456", provider: "external", wantErr: "provider"},
		{name: "release verified lease", id: "cbx_abcdef123456", provider: "aws", wantRelease: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnv(t)
			isolateTestUserDirs(t)
			releases := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+test.id:
					if test.leaseStatus != 0 {
						http.Error(w, `{"error":"inspect_failed"}`, test.leaseStatus)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
						ID: test.id, Provider: test.provider, State: "active",
					}})
				case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/"+test.id+"/release":
					releases++
					_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
						ID: test.id, Provider: "aws", State: "released",
					}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "user-token")

			err := (App{Stdout: io.Discard, Stderr: io.Discard}).stop(context.Background(), []string{
				"--provider", "aws", "--id", test.id, "--force",
			})
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("stop --force err=%v, want %q", err, test.wantErr)
			}
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if releases != test.wantRelease {
				t.Fatalf("release requests=%d want %d", releases, test.wantRelease)
			}
		})
	}
}

func TestCoordinatorAcquireProviderMismatchCleanupPolicy(t *testing.T) {
	for _, test := range []struct {
		name        string
		fixed       bool
		wantLease   string
		wantPath    string
		wantPut     bool
		wantCancels int
	}{
		{name: "new non-fixed lease cancels requested create", wantLease: "cbx_created_identity", wantPath: "/v1/leases", wantCancels: 1},
		{name: "fixed lease is retained", fixed: true, wantLease: "cbx_fixed_identity", wantPath: "/v1/leases/cbx_fixed_identity", wantPut: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateTestUserDirs(t)
			releases := 0
			cancellations := 0
			var capturedRequestedID, createAttemptID string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == test.wantPath && ((!test.wantPut && r.Method == http.MethodPost) || (test.wantPut && r.Method == http.MethodPut)):
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					capturedRequestedID, _ = body["leaseID"].(string)
					createAttemptID, _ = body["createAttemptID"].(string)
					_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
						ID: test.wantLease, Provider: "external", TargetOS: targetLinux, State: "active",
					}})
				case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/"+capturedRequestedID+"/cancel-create":
					cancellations++
					_ = json.NewEncoder(w).Encode(map[string]any{"canceledCreate": CoordinatorCanceledCreateAttestation{
						Version: 1, RequestedLeaseID: capturedRequestedID, CreateAttemptID: createAttemptID, State: "canceled",
					}})
				case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/"+test.wantLease+"/release":
					releases++
					_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: test.wantLease, State: "released"}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			backend := newCoordinatorIdentityTestBackend(t, server.URL, "")
			backend.cfg.AWSSSHCIDRs = []string{"0.0.0.0/0"}
			operationID := ""
			if test.fixed {
				operationID = test.wantLease
			}
			_, err := backend.acquireOnceWithLeaseID(context.Background(), false, operationID, "identity-fence")
			assertCoordinatorProviderIdentityError(t, err, "external", test.wantLease)
			if cancellations != test.wantCancels {
				t.Fatalf("cancel-create requests=%d want %d", cancellations, test.wantCancels)
			}
			if releases != 0 {
				t.Fatalf("release requests=%d want zero", releases)
			}
		})
	}
}

type coordinatorIdentityRecordingBackend struct {
	testSSHBackend
	resolveCalls atomic.Int32
}

func (b *coordinatorIdentityRecordingBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	b.resolveCalls.Add(1)
	return LeaseTarget{}, errors.New("direct provider resolve must not run")
}

func TestActionsResolveRejectsCoordinatorProviderMismatchBeforeClaimOrLocalAdapter(t *testing.T) {
	isolateTestUserDirs(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/leases/cbx_actions_identity" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
			ID:       "cbx_actions_identity",
			Slug:     "actions-identity",
			Provider: "external",
			State:    "active",
			CloudID:  "external-workspace",
			Host:     "203.0.113.20",
		}})
	}))
	defer server.Close()

	direct := &coordinatorIdentityRecordingBackend{
		testSSHBackend: testSSHBackend{spec: ProviderSpec{Name: "aws"}},
	}
	testAWSBackendOverride = direct
	t.Cleanup(func() { testAWSBackendOverride = nil })
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.providerSelectionSource = providerSelectionFlag
	cfg.TargetOS = targetLinux
	cfg.Coordinator = server.URL
	cfg.CoordToken = "user-token"

	_, _, _, _, err := (App{Stderr: io.Discard}).resolveLeaseTargetForActions(
		context.Background(),
		cfg,
		"cbx_actions_identity",
		Repo{Root: t.TempDir(), Name: "identity-test"},
		false,
	)
	assertCoordinatorProviderIdentityError(t, err, "external", "cbx_actions_identity")
	if direct.resolveCalls.Load() != 0 {
		t.Fatalf("local provider resolve calls=%d want zero", direct.resolveCalls.Load())
	}
	if _, exists, readErr := readLeaseClaimWithPresence("cbx_actions_identity"); readErr != nil {
		t.Fatal(readErr)
	} else if exists {
		t.Fatal("provider mismatch wrote a local lease claim")
	}
}

func newCoordinatorIdentityTestBackend(t *testing.T, serverURL, adminToken string) *coordinatorLeaseBackend {
	t.Helper()
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetLinux
	cfg.Coordinator = serverURL
	cfg.CoordToken = "user-token"
	cfg.CoordAdminToken = adminToken
	coord, _, err := newCoordinatorClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &coordinatorLeaseBackend{
		spec:  ProviderSpec{Name: "aws"},
		cfg:   cfg,
		coord: coord,
		rt:    Runtime{Stderr: io.Discard},
	}
}

func assertCoordinatorProviderIdentityError(t *testing.T, err error, returnedProvider, leaseID string) {
	t.Helper()
	if !isCoordinatorProviderIdentityError(err) {
		t.Fatalf("error=%v, want typed coordinator provider identity error", err)
	}
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 4 {
		t.Fatalf("error=%v, want exit 4 provider identity mismatch", err)
	}
	for _, want := range []string{
		"selected_provider=aws",
		"returned_provider=" + returnedProvider,
		"lease_id=" + leaseID,
	} {
		if !strings.Contains(exitErr.Message, want) {
			t.Fatalf("diagnostic=%q missing %q", exitErr.Message, want)
		}
	}
}

func TestCoordinatorReleaseFallsBackToAdminToken(t *testing.T) {
	isolateTestUserDirs(t)
	configureCoordinatorReleaseTestTiming(t, time.Second, 0)
	adminReleased := false
	adminObservations := 0
	var payloads []map[string]any
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/leases/cbx_admin" {
			if r.Header.Get("Authorization") != "Bearer admin-token" {
				t.Fatalf("observation auth=%q, want admin token", r.Header.Get("Authorization"))
			}
			adminObservations++
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: "cbx_admin", Provider: "aws", State: "released"}})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/admin/leases/cbx_admin/release" && r.URL.Path != "/v1/leases/cbx_admin/release" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, body)
		paths = append(paths, r.URL.Path)
		switch r.Header.Get("Authorization") {
		case "Bearer user-token":
			http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
		case "Bearer admin-token":
			if r.URL.Path != "/v1/admin/leases/cbx_admin/release" {
				t.Fatalf("admin release path=%s", r.URL.Path)
			}
			adminReleased = true
			deleting := true
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: "cbx_admin", Provider: "aws", State: "released", CleanupStartedAt: "2026-08-19T00:00:00Z", ReleaseDeletesServer: &deleting}})
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

	err = backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{
		LeaseID: "cbx_admin", Server: Server{Provider: "aws"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !adminReleased {
		t.Fatal("admin release was not called")
	}
	if adminObservations != 1 {
		t.Fatalf("admin observations=%d want 1", adminObservations)
	}
	if len(payloads) != 6 || paths[len(paths)-1] != "/v1/admin/leases/cbx_admin/release" {
		t.Fatalf("release paths=%#v payloads=%#v", paths, payloads)
	}
	for i, payload := range payloads {
		if payload["expectedProvider"] != "aws" {
			t.Fatalf("release payload %d=%#v, want expectedProvider=aws", i, payload)
		}
	}
}

func TestCoordinatorAcquireRollbackQueuesReleaseOnceWithoutObservation(t *testing.T) {
	isolateTestUserDirs(t)
	var releasePosts, observations int
	leaseID := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			leaseID, _ = body["leaseID"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: leaseID, Provider: "aws", TargetOS: targetLinux, State: "active", Desktop: false,
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/"+leaseID+"/release":
			releasePosts++
			deleting := true
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: leaseID, Provider: "aws", State: "released", CleanupStartedAt: "2026-08-19T00:00:00Z", ReleaseDeletesServer: &deleting,
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+leaseID:
			observations++
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: leaseID, Provider: "aws", State: "released"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetLinux
	cfg.Desktop = true
	cfg.AWSSSHCIDRs = []string{"127.0.0.1/32"}
	cfg.Coordinator = server.URL
	cfg.CoordToken = "user-token"
	coord, _, err := newCoordinatorClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	backend := &coordinatorLeaseBackend{spec: ProviderSpec{Name: "aws"}, cfg: cfg, coord: coord, rt: Runtime{Stderr: &stderr}}
	_, err = backend.acquireOnceWithLeaseID(context.Background(), false, "", "rollback-test")
	if err == nil || !strings.Contains(err.Error(), "did not provision desktop=true") {
		t.Fatalf("acquire error=%v, want capability mismatch", err)
	}
	if releasePosts != 1 || observations != 0 {
		t.Fatalf("rollback release POSTs=%d observations=%d want 1/0", releasePosts, observations)
	}
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(keyPath)); err != nil {
		t.Fatalf("pending rollback removed local SSH artifacts: %v", err)
	}
	if got := stderr.String(); !strings.Contains(got, "did not confirm provider cleanup (is still pending)") || !strings.Contains(got, leaseID) {
		t.Fatalf("pending rollback warning missing: %q", got)
	}
}

func TestCoordinatorAcquireRollbackReportsNonFinalRelease(t *testing.T) {
	deleting := true
	retained := false
	tests := []struct {
		name     string
		lease    CoordinatorLease
		release  error
		contains string
	}{
		{name: "retained", lease: CoordinatorLease{State: "released", ReleaseDeletesServer: &retained}, contains: "retained the provider resource"},
		{name: "cleanup failed", lease: CoordinatorLease{State: "released", CleanupError: "sensitive provider detail", ReleaseDeletesServer: &deleting}, contains: "reported a cleanup failure or scheduled retry"},
		{name: "cleanup retry", lease: CoordinatorLease{State: "released", CleanupRetryAt: "2026-08-19T00:05:00Z", ReleaseDeletesServer: &deleting}, contains: "reported a cleanup failure or scheduled retry"},
		{name: "unexpected", lease: CoordinatorLease{State: "active"}, contains: "returned an unexpected non-final state"},
		{name: "request failed", release: errors.New("release request failed"), contains: "release failed after test rollback"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			reportCoordinatorAcquisitionRollback(&stderr, "cbx_abcdef123456", "test rollback", tc.lease, tc.release)
			got := stderr.String()
			if !strings.Contains(got, tc.contains) || !strings.Contains(got, "cbx_abcdef123456") {
				t.Fatalf("rollback warning=%q, want %q and lease ID", got, tc.contains)
			}
			if strings.Contains(got, "sensitive provider detail") {
				t.Fatalf("rollback warning leaked coordinator cleanup detail: %q", got)
			}
		})
	}
}

func TestCoordinatorAcquireCancelsStaleInstanceLease(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var createdLeaseID string
	var createAttemptID string
	cancellations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			createdLeaseID, _ = body["leaseID"].(string)
			createAttemptID, _ = body["createAttemptID"].(string)
			http.Error(w, `{"error":"InvalidInstanceID.NotFound: instance disappeared"}`, http.StatusInternalServerError)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel-create"):
			cancellations++
			if createdLeaseID == "" || !strings.Contains(r.URL.Path, createdLeaseID) {
				t.Fatalf("cancel path=%s created=%s", r.URL.Path, createdLeaseID)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"canceledCreate": map[string]any{
				"version": 1, "requestedLeaseID": createdLeaseID,
				"createAttemptID": createAttemptID, "state": "canceled",
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
	if cancellations != 1 {
		t.Fatalf("cancel-create requests=%d want 1", cancellations)
	}
	if !strings.Contains(stderr.String(), "recorded canceled coordinator create") {
		t.Fatalf("missing cancel-create warning: %q", stderr.String())
	}
}

func TestCoordinatorAcquireRetriesStaleInstanceAfterTokenCancellation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	creates := 0
	cancellations := 0
	var leaseID, createAttemptID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
			creates++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if creates > 1 {
				http.Error(w, `{"error":"capacity exhausted after retry"}`, http.StatusConflict)
				return
			}
			leaseID, _ = body["leaseID"].(string)
			createAttemptID, _ = body["createAttemptID"].(string)
			http.Error(w, `{"error":"InvalidInstanceID.NotFound: instance disappeared"}`, http.StatusInternalServerError)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel-create"):
			cancellations++
			_ = json.NewEncoder(w).Encode(map[string]any{"canceledCreate": map[string]any{
				"version": 1, "requestedLeaseID": leaseID,
				"createAttemptID": createAttemptID, "state": "canceled",
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

	_, err = backend.Acquire(context.Background(), AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "capacity exhausted after retry") {
		t.Fatalf("err=%v", err)
	}
	if creates != 2 {
		t.Fatalf("creates=%d want 2", creates)
	}
	if cancellations != 1 {
		t.Fatalf("cancel-create requests=%d want 1", cancellations)
	}
	if !strings.Contains(stderr.String(), "recorded canceled coordinator create") {
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

func TestCoordinatorInspectJSONPreservesProviderCleanupReceipt(t *testing.T) {
	isolateTestUserDirs(t)
	clearConfigEnv(t)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "user-token")
	for _, state := range []string{"running", "success", "historical"} {
		t.Run(state, func(t *testing.T) {
			receipt := map[string]any{
				"version": 1, "provider": "hetzner", "leaseID": "cbx_abcdef123456", "serverID": 123,
				"dispatchStartedAt": "2026-09-01T08:00:00Z",
				"action":            map[string]any{"id": 456, "status": state},
			}
			if state == "success" {
				receipt["confirmation"] = map[string]any{"method": "delete-action-success-and-server-absent", "at": "2026-09-01T08:00:02Z"}
			}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodGet || r.URL.Path != "/v1/leases/cbx_abcdef123456" || r.Header.Get("Authorization") != "Bearer user-token" {
					t.Errorf("unexpected broker inspect request: %s %s", r.Method, r.URL.Path)
				}
				lease := map[string]any{"id": "cbx_abcdef123456", "provider": "hetzner", "state": "released", "cloudID": "123", "serverID": 123, "host": "192.0.2.1", "cleanupStatus": "pending"}
				if state != "historical" {
					lease["providerCleanup"] = receipt
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
			}))
			defer server.Close()
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			var stdout bytes.Buffer
			app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
			if err := app.inspect(context.Background(), []string{"--provider", "hetzner", "--id", "cbx_abcdef123456", "--json"}); err != nil {
				t.Fatal(err)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if requests != 1 {
				t.Fatalf("inspect made %d requests, want one broker GET", requests)
			}
			if state == "historical" {
				if _, exists := got["providerCleanup"]; exists {
					t.Fatal("historical receipt was invented")
				}
				return
			}
			want, _ := json.Marshal(receipt)
			var actualValue, expectedValue any
			_ = json.Unmarshal(got["providerCleanup"], &actualValue)
			_ = json.Unmarshal(want, &expectedValue)
			if !reflect.DeepEqual(actualValue, expectedValue) {
				t.Fatalf("receipt = %s, want %s", got["providerCleanup"], want)
			}
			if string(got["cleanupStatus"]) != `"pending"` || string(got["ready"]) != "false" || string(got["hasHost"]) != "true" {
				t.Fatalf("recorded evidence changed finality or historical host semantics: %s", stdout.Bytes())
			}
		})
	}
}
