package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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

func TestCoordinatorInspectJSONIncludesOptionalSSHHostKey(t *testing.T) {
	const sshHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILocalAuthoritativeHostKey"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/v1/leases/") {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("providerMetadata"); got != "authoritative" {
			t.Fatalf("providerMetadata=%q, want authoritative", got)
		}
		lease := CoordinatorLease{
			ID:               strings.TrimPrefix(r.URL.Path, "/v1/leases/"),
			Provider:         "aws",
			TargetOS:         targetLinux,
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
		{id: "cbx_without_key", configuredWith: "daytona", wantKey: false},
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

func TestCoordinatorCreateLeaseCancellationReleasesLateAcceptedLease(t *testing.T) {
	oldRecoveryTimeout := coordinatorCanceledCreateRecoveryTimeout
	oldRecoveryInterval := coordinatorCreateLeaseRecoveryInterval
	coordinatorCanceledCreateRecoveryTimeout = time.Second
	coordinatorCreateLeaseRecoveryInterval = 10 * time.Millisecond
	defer func() {
		coordinatorCanceledCreateRecoveryTimeout = oldRecoveryTimeout
		coordinatorCreateLeaseRecoveryInterval = oldRecoveryInterval
	}()

	createStarted := make(chan struct{})
	allowCreateCommit := make(chan struct{})
	leaseAccepted := make(chan struct{})
	firstReconcileRelease := make(chan struct{})
	releaseObserved := make(chan struct{})
	var firstReleaseAttempt sync.Once
	var firstRelease sync.Once
	var releaseCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
			close(createStarted)
			<-allowCreateCommit // Model a coordinator/provider path that commits after client cancellation.
			close(leaseAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID:       "cbx_cancel_late",
				Slug:     "pearl-prawn",
				Provider: "aws",
				TargetOS: targetLinux,
				Host:     "203.0.113.44",
				State:    "active",
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/cbx_cancel_late/release":
			select {
			case <-leaseAccepted:
			default:
				firstReleaseAttempt.Do(func() { close(firstReconcileRelease) })
				http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
				return
			}
			var body struct {
				Delete bool `json:"delete"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if !body.Delete {
				t.Error("late lease release did not request provider deletion")
			}
			releaseCount.Add(1)
			firstRelease.Do(func() { close(releaseObserved) })
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID:       "cbx_cancel_late",
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

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan coordinatorCreateLeaseResult, 1)
	go func() {
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
	<-createStarted
	cancel()

	select {
	case <-firstReconcileRelease:
	case <-time.After(250 * time.Millisecond):
		close(allowCreateCommit)
		t.Fatal("canceled create did not reconcile its pre-generated lease id with delete-release")
	}
	close(allowCreateCommit)

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
		t.Fatalf("late accepted lease was not released; stderr=%q", stderr.String())
	}
	if got := releaseCount.Load(); got != 1 {
		t.Fatalf("release requests=%d, want 1", got)
	}
}

func TestCanceledCoordinatorCreateRetriesTransientReleaseFailure(t *testing.T) {
	oldRecoveryInterval := coordinatorCreateLeaseRecoveryInterval
	coordinatorCreateLeaseRecoveryInterval = time.Millisecond
	defer func() { coordinatorCreateLeaseRecoveryInterval = oldRecoveryInterval }()

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/cbx_cancel_transient/release" {
			http.NotFound(w, r)
			return
		}
		if attempts.Add(1) == 1 {
			http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
			ID: "cbx_cancel_transient", Slug: "retry-crab", State: "released",
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

	if err := backend.releaseCoordinatorLeaseAfterCanceledCreate(recoverCtx, "cbx_cancel_transient", "retry-crab"); err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("delete-release attempts=%d, want transient failure plus retry", got)
	}
}

func TestCanceledCoordinatorCreateMakesOneFreshFinalReleaseAttemptAtRecoveryDeadline(t *testing.T) {
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
			if req.Method != http.MethodPost || req.URL.Path != "/v1/leases/cbx_cancel_final/release" {
				return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
			}
			deadline, ok := req.Context().Deadline()
			if !ok {
				return nil, errors.New("delete-release attempt had no deadline")
			}
			if attempt > len(deadlines) {
				return nil, fmt.Errorf("unexpected delete-release attempt %d", attempt)
			}
			deadlines[attempt-1] = deadline
			if attempt == 1 {
				<-req.Context().Done()
				return nil, req.Context().Err()
			}
			if err := req.Context().Err(); err != nil {
				return nil, fmt.Errorf("final delete-release started with expired context: %w", err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"lease":{"id":"cbx_cancel_final","slug":"final-crab","state":"released"}}`,
				)),
				Request: req,
			}, nil
		})},
	}
	backend := &coordinatorLeaseBackend{coord: coord, rt: Runtime{Stderr: &bytes.Buffer{}}}
	recoverCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if err := backend.releaseCoordinatorLeaseAfterCanceledCreate(recoverCtx, "cbx_cancel_final", "final-crab"); err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("delete-release attempts=%d, want recovery attempt plus exactly one final attempt", got)
	}
	if !deadlines[1].After(deadlines[0]) {
		t.Fatalf("final deadline=%s, want fresh deadline after expired recovery deadline=%s", deadlines[1], deadlines[0])
	}
}

func TestCoordinatorFixedCreateCancellationDoesNotReleaseDurableLease(t *testing.T) {
	putStarted := make(chan struct{})
	allowPutReturn := make(chan struct{})
	putFinished := make(chan struct{})
	var releases atomic.Int32
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
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/cbx_fixed_cancel/release":
			releases.Add(1)
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
	if got := releases.Load(); got != 0 {
		t.Fatalf("fixed replay-owned lease was released %d time(s)", got)
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
	backend := &coordinatorLeaseBackend{rt: Runtime{Stderr: &bytes.Buffer{}}}

	err := backend.canceledCoordinatorLeaseCreateError(ctx, "cbx_definitive_cancel", "amber-crab", false, createErr)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want caller cancellation", err)
	}
	var gotHTTPError CoordinatorHTTPError
	if !errors.As(err, &gotHTTPError) || gotHTTPError.StatusCode != http.StatusBadRequest {
		t.Fatalf("err=%v, want joined definitive create error", err)
	}
}

func TestCanceledCoordinatorCreateDeletesReleasedRetainedLease(t *testing.T) {
	var gets atomic.Int32
	var releases atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/cbx_cancel_retained":
			gets.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID:       "cbx_cancel_retained",
				Slug:     "coral-crab",
				Provider: "aws",
				State:    "released",
				Keep:     true,
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/cbx_cancel_retained/release":
			var body struct {
				Delete bool `json:"delete"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if !body.Delete {
				t.Error("retained lease release did not request provider deletion")
			}
			releases.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID:       "cbx_cancel_retained",
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
	backend := &coordinatorLeaseBackend{
		cfg:   cfg,
		coord: coord,
		rt:    Runtime{Stderr: &bytes.Buffer{}},
	}

	if err := backend.releaseCoordinatorLeaseAfterCanceledCreate(
		context.Background(),
		"cbx_cancel_retained",
		"coral-crab",
	); err != nil {
		t.Fatal(err)
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("released retained lease release requests=%d, want 1", got)
	}
	if got := gets.Load(); got != 0 {
		t.Fatalf("released retained lease GET requests=%d, want direct release", got)
	}
}

func TestCanceledCoordinatorCreateRequiresRequestedLeaseIDAttestationForCanonicalRelease(t *testing.T) {
	tests := []struct {
		name           string
		requestLeaseID any
		wantError      bool
	}{
		{name: "correct", requestLeaseID: "cbx_cancel_expected"},
		{name: "incorrect", requestLeaseID: "cbx_cancel_other", wantError: true},
		{name: "missing", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/cbx_cancel_expected/release" {
					http.NotFound(w, r)
					return
				}
				response := map[string]any{"lease": CoordinatorLease{
					ID: "cbx_cancel_canonical", State: "released",
				}}
				if tt.requestLeaseID != nil {
					response["requestLeaseID"] = tt.requestLeaseID
				}
				_ = json.NewEncoder(w).Encode(response)
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

			err = backend.releaseCoordinatorLeaseAfterCanceledCreate(
				context.Background(),
				"cbx_cancel_expected",
				"expected-crab",
			)
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "without matching requestLeaseID attestation") {
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
	releases := 0
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
	if lease.ID != createdLeaseID || lease.Host != "203.0.113.44" {
		t.Fatalf("lease=%#v created=%s", lease, createdLeaseID)
	}
	if gets == 0 {
		t.Fatal("expected recovery GET")
	}
	if releases != 0 {
		t.Fatalf("active uncertain create release requests=%d, want 0", releases)
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

func TestCoordinatorFixedCreateCommitThenTimeoutRepeatsPutAndCreatesOnce(t *testing.T) {
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
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/leases/cbx_abcdef123463":
			puts++
			if puts == 1 {
				creates++
				http.Error(w, "error code: 1101", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: "cbx_abcdef123463", Slug: "fixed-commit", Provider: "aws", TargetOS: targetLinux, State: "provisioning",
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/cbx_abcdef123463":
			gets++
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: "cbx_abcdef123463", Slug: "fixed-commit", Provider: "aws", TargetOS: targetLinux,
				State: "active", CloudID: "i-fixed", Host: "203.0.113.44",
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
		"cbx_abcdef123463", "fixed-commit", true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if lease.ID != "cbx_abcdef123463" || lease.CloudID != "i-fixed" || puts != 2 || gets == 0 || creates != 1 {
		t.Fatalf("lease=%#v puts=%d gets=%d creates=%d", lease, puts, gets, creates)
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
	isolateTestUserDirs(t)
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
