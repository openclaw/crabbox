package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHeartbeatIdentifierSyntax(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		valid  bool
		wantID string
	}{
		{name: "positional", args: []string{"--provider", heartbeatDirectProviderName, "direct-heartbeat"}, valid: true, wantID: "cbx_direct_heartbeat"},
		{name: "id flag", args: []string{"--provider", heartbeatDirectProviderName, "--id", "direct-heartbeat"}, valid: true, wantID: "cbx_direct_heartbeat"},
		{name: "repeated id flags", args: []string{"--provider", heartbeatDirectProviderName, "--id", "first", "--id", "second"}},
		{name: "repeated id equals flags", args: []string{"--provider", heartbeatDirectProviderName, "--id=first", "--id=second"}},
		{name: "id flag and one positional", args: []string{"--provider", heartbeatDirectProviderName, "--id", "direct-heartbeat", "extra"}},
		{name: "id flag and two positionals", args: []string{"--provider", heartbeatDirectProviderName, "--id", "direct-heartbeat", "extra", "second"}},
		{name: "two positionals", args: []string{"--provider", heartbeatDirectProviderName, "direct-heartbeat", "extra"}},
		{name: "missing identifier", args: []string{"--provider", heartbeatDirectProviderName}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnv(t)
			stateRoot := t.TempDir()
			t.Setenv("XDG_STATE_HOME", stateRoot)

			backend := &heartbeatDirectBackend{lease: heartbeatDirectTestLease("cbx_direct_heartbeat", "direct-heartbeat")}
			heartbeatDirectBackendForTest = backend
			t.Cleanup(func() { heartbeatDirectBackendForTest = nil })

			var coordinatorRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				coordinatorRequests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/cbx_direct_heartbeat/heartbeat" {
					t.Errorf("request=%s %s, want registered heartbeat POST", r.Method, r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
					ID: "cbx_direct_heartbeat", Slug: "direct-heartbeat", Provider: heartbeatDirectProviderName, State: "active",
				}})
			}))
			defer server.Close()

			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if test.valid {
				config := fmt.Sprintf("provider: %s\nbroker:\n  url: %s\n  mode: registered\n", heartbeatDirectProviderName, server.URL)
				if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
					t.Fatal(err)
				}
				cfg := defaultConfig()
				cfg.Provider = heartbeatDirectProviderName
				if err := claimLeaseTargetForRepoConfig(backend.lease.LeaseID, serverSlug(backend.lease.Server), cfg, backend.lease.Server, SSHTarget{}, "/repo", 30*time.Minute, false); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(configPath, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "user-token")

			before := captureClaimsListState(t, stateRoot)
			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), append([]string{"heartbeat"}, test.args...))
			if test.valid {
				if err != nil {
					t.Fatalf("heartbeat error=%v stderr=%q", err, stderr.String())
				}
				if coordinatorRequests.Load() != 1 || backend.resolves != 1 || len(backend.touches) != 1 {
					t.Fatalf("side effects: requests=%d configures=%d resolves=%d touches=%d", coordinatorRequests.Load(), backend.configures, backend.resolves, len(backend.touches))
				}
				if !strings.Contains(stdout.String(), "heartbeat lease="+test.wantID) {
					t.Fatalf("heartbeat output=%q", stdout.String())
				}
				return
			}

			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.HasPrefix(exitErr.Message, "usage: crabbox heartbeat") {
				t.Fatalf("error=%v, want heartbeat usage with exit 2", err)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("invalid syntax wrote stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if backend.configures != 0 || backend.resolves != 0 || len(backend.touches) != 0 || coordinatorRequests.Load() != 0 {
				t.Fatalf("invalid syntax crossed side-effect boundary: requests=%d configures=%d resolves=%d touches=%d", coordinatorRequests.Load(), backend.configures, backend.resolves, len(backend.touches))
			}
			after := captureClaimsListState(t, stateRoot)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("invalid syntax mutated claim state\nbefore=%#v\nafter=%#v", before, after)
			}
		})
	}
}

func TestHeartbeatCoordinatorPassesIdleTimeoutAndPrintsLeaseState(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/blue-lobster/heartbeat" {
			t.Fatalf("request=%s %s, want heartbeat POST", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer user-token" {
			t.Fatalf("authorization=%q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
			ID:                 "cbx_heartbeat",
			Slug:               "blue-lobster",
			Provider:           "aws",
			State:              "active",
			LastTouchedAt:      "2026-08-16T20:00:00Z",
			IdleTimeoutSeconds: 5400,
			ExpiresAt:          "2026-08-16T21:30:00Z",
		}})
	}))
	defer server.Close()

	configureHeartbeatCoordinatorTest(t, server.URL)
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{
		"heartbeat", "--provider", "aws", "--id", "blue-lobster", "--idle-timeout", "90m", "--json",
	})
	if err != nil {
		t.Fatalf("heartbeat error=%v stderr=%s", err, stderr.String())
	}
	if requestBody["expectedProvider"] != "aws" || requestBody["idleTimeoutSeconds"] != float64(5400) {
		t.Fatalf("heartbeat body=%v", requestBody)
	}
	var got leaseHeartbeatView
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "cbx_heartbeat" || got.Slug != "blue-lobster" || got.Provider != "aws" || got.State != "active" || got.IdleTimeout != "1h30m0s" {
		t.Fatalf("heartbeat output=%#v", got)
	}
}

func TestHeartbeatCoordinatorOmitsIdleTimeoutWithoutOverride(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body["idleTimeoutSeconds"]; ok {
			t.Fatalf("heartbeat body unexpectedly included idle timeout: %v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
			ID: "cbx_heartbeat", Slug: "blue-lobster", Provider: "aws", State: "active",
		}})
	}))
	defer server.Close()

	configureHeartbeatCoordinatorTest(t, server.URL)
	var stdout bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: &bytes.Buffer{}}).heartbeat(context.Background(), []string{"--provider", "aws", "--id", "blue-lobster"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "heartbeat lease=cbx_heartbeat slug=blue-lobster provider=aws state=active") {
		t.Fatalf("heartbeat output=%q", stdout.String())
	}
}

func TestHeartbeatCoordinatorFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name       string
		statusCode int
		errorCode  string
		message    string
	}{
		{name: "not owner", statusCode: http.StatusForbidden, errorCode: "forbidden", message: "lease manage access required"},
		{name: "unknown lease", statusCode: http.StatusNotFound, errorCode: "not_found", message: "not found"},
		{name: "terminal lease", statusCode: http.StatusConflict, errorCode: "lease_ended", message: "lease has already ended"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.statusCode)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": test.errorCode, "message": test.message})
			}))
			defer server.Close()

			configureHeartbeatCoordinatorTest(t, server.URL)
			var stdout bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &bytes.Buffer{}}).heartbeat(context.Background(), []string{"--provider", "aws", "--id", "missing-or-foreign"})
			if err == nil {
				t.Fatal("heartbeat unexpectedly succeeded")
			}
			for _, want := range []string{
				"coordinator POST /v1/leases/missing-or-foreign/heartbeat",
				"http " + strconv.Itoa(test.statusCode),
				test.message,
			} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error=%q, want %q", err, want)
				}
			}
			if stdout.Len() != 0 {
				t.Fatalf("failed heartbeat output=%q", stdout.String())
			}
		})
	}
}

func TestHeartbeatRegisteredModeUsesCoordinator(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/leases/cbx_registered/heartbeat" {
			t.Fatalf("registered heartbeat path=%q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["expectedProvider"] != heartbeatDirectProviderName || body["idleTimeoutSeconds"] != float64(3600) {
			t.Fatalf("registered heartbeat body=%v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
			ID: "cbx_registered", Slug: "registered-heartbeat", Provider: heartbeatDirectProviderName, State: "active", IdleTimeoutSeconds: 3600,
		}})
	}))
	defer server.Close()

	clearConfigEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	config := fmt.Sprintf("provider: %s\nbroker:\n  url: %s\n  mode: registered\n", heartbeatDirectProviderName, server.URL)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "user-token")
	backend := &heartbeatDirectBackend{}
	backend.lease = heartbeatDirectTestLease("cbx_registered", "registered-heartbeat")
	heartbeatDirectBackendForTest = backend
	t.Cleanup(func() { heartbeatDirectBackendForTest = nil })
	cfg := defaultConfig()
	cfg.Provider = heartbeatDirectProviderName
	if err := claimLeaseTargetForRepoConfig(backend.lease.LeaseID, serverSlug(backend.lease.Server), cfg, backend.lease.Server, SSHTarget{}, "/repo", 30*time.Minute, false); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: &bytes.Buffer{}}).heartbeat(context.Background(), []string{
		"--id", "registered-heartbeat", "--idle-timeout", "1h", "--json",
	}); err != nil {
		t.Fatal(err)
	}
	if len(backend.touches) != 1 || backend.touches[0].IdleTimeout != time.Hour || backend.touches[0].IdleTimeoutOverride == nil || *backend.touches[0].IdleTimeoutOverride != time.Hour {
		t.Fatalf("registered heartbeat direct touches=%#v", backend.touches)
	}
}

func TestHeartbeatRejectsProviderWithoutLeaseTouch(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	err := (App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).heartbeat(context.Background(), []string{
		"--provider", "service-control-test", "--id", "configured-app",
	})
	if err == nil || !strings.Contains(err.Error(), "provider=service-control-test does not support lease heartbeat") {
		t.Fatalf("error=%v", err)
	}
}

func configureHeartbeatCoordinatorTest(t *testing.T, serverURL string) {
	t.Helper()
	clearConfigEnv(t)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", serverURL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "user-token")
}

const heartbeatDirectProviderName = "heartbeat-direct-test"

var heartbeatDirectBackendForTest *heartbeatDirectBackend

func init() {
	RegisterProvider(heartbeatDirectProvider{})
}

type heartbeatDirectProvider struct{}

func (heartbeatDirectProvider) Name() string      { return heartbeatDirectProviderName }
func (heartbeatDirectProvider) Aliases() []string { return nil }
func (heartbeatDirectProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        heartbeatDirectProviderName,
		Family:      "heartbeat-test",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Coordinator: CoordinatorNever,
	}
}
func (heartbeatDirectProvider) RegisterFlags(*flag.FlagSet, Config) any { return noProviderFlags{} }
func (heartbeatDirectProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (heartbeatDirectProvider) Configure(Config, Runtime) (Backend, error) {
	if heartbeatDirectBackendForTest == nil {
		return nil, errors.New("heartbeat direct test backend is not configured")
	}
	heartbeatDirectBackendForTest.configures++
	return heartbeatDirectBackendForTest, nil
}

type heartbeatDirectBackend struct {
	lease      LeaseTarget
	configures int
	resolves   int
	touches    []TouchRequest
}

func (*heartbeatDirectBackend) Spec() ProviderSpec { return heartbeatDirectProvider{}.Spec() }
func (b *heartbeatDirectBackend) Acquire(context.Context, AcquireRequest) (LeaseTarget, error) {
	return b.lease, nil
}
func (b *heartbeatDirectBackend) Resolve(_ context.Context, req ResolveRequest) (LeaseTarget, error) {
	b.resolves++
	if req.ID != b.lease.LeaseID && req.ID != serverSlug(b.lease.Server) {
		return LeaseTarget{}, fmt.Errorf("lease %s not found", req.ID)
	}
	return b.lease, nil
}
func (*heartbeatDirectBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}
func (b *heartbeatDirectBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	b.touches = append(b.touches, req)
	server := req.Lease.Server
	server.Labels = cloneStringMap(server.Labels)
	server.Labels["last_touched_at"] = "2026-08-16T20:00:00Z"
	server.Labels["idle_timeout_secs"] = durationSecondsLabel(req.IdleTimeout)
	server.Labels["expires_at"] = "2026-08-16T21:30:00Z"
	return server, nil
}
func (*heartbeatDirectBackend) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	return nil
}

func TestHeartbeatDirectProviderUsesTouchForExactClaim(t *testing.T) {
	backend := configureHeartbeatDirectTest(t, true)
	var stdout bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: &bytes.Buffer{}}).heartbeat(context.Background(), []string{
		"--provider", heartbeatDirectProviderName, "--id", "direct-heartbeat", "--idle-timeout", "45m", "--json",
	}); err != nil {
		t.Fatal(err)
	}
	if len(backend.touches) != 1 || backend.touches[0].IdleTimeout != 45*time.Minute || backend.touches[0].IdleTimeoutOverride == nil || *backend.touches[0].IdleTimeoutOverride != 45*time.Minute {
		t.Fatalf("touches=%#v", backend.touches)
	}
	var got leaseHeartbeatView
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != backend.lease.LeaseID || got.IdleTimeout != "45m0s" || got.LastTouchedAt != "2026-08-16T20:00:00Z" {
		t.Fatalf("heartbeat output=%#v", got)
	}
}

func TestHeartbeatDirectProviderOmitsIdleTimeoutOverrideIntent(t *testing.T) {
	backend := configureHeartbeatDirectTest(t, true)
	if err := (App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).heartbeat(context.Background(), []string{
		"--provider", heartbeatDirectProviderName, "--id", "direct-heartbeat",
	}); err != nil {
		t.Fatal(err)
	}
	if len(backend.touches) != 1 || backend.touches[0].IdleTimeoutOverride != nil {
		t.Fatalf("omitted timeout carried replacement intent: %#v", backend.touches)
	}
}

func TestHeartbeatDirectProviderRejectsClaimlessLease(t *testing.T) {
	backend := configureHeartbeatDirectTest(t, false)
	err := (App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).heartbeat(context.Background(), []string{
		"--provider", heartbeatDirectProviderName, "--id", "direct-heartbeat",
	})
	if err == nil || !strings.Contains(err.Error(), "is not claimed for provider="+heartbeatDirectProviderName) {
		t.Fatalf("error=%v", err)
	}
	if len(backend.touches) != 0 {
		t.Fatalf("claimless heartbeat touched lease: %#v", backend.touches)
	}
}

func configureHeartbeatDirectTest(t *testing.T, claim bool) *heartbeatDirectBackend {
	t.Helper()
	clearConfigEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("provider: "+heartbeatDirectProviderName+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lease := heartbeatDirectTestLease("cbx_direct_heartbeat", "direct-heartbeat")
	server := lease.Server
	backend := &heartbeatDirectBackend{lease: lease}
	heartbeatDirectBackendForTest = backend
	t.Cleanup(func() { heartbeatDirectBackendForTest = nil })
	if claim {
		cfg := defaultConfig()
		cfg.Provider = heartbeatDirectProviderName
		if err := claimLeaseTargetForRepoConfig(backend.lease.LeaseID, serverSlug(server), cfg, server, SSHTarget{}, "/repo", 30*time.Minute, false); err != nil {
			t.Fatal(err)
		}
	}
	return backend
}

func heartbeatDirectTestLease(leaseID, slug string) LeaseTarget {
	return LeaseTarget{LeaseID: leaseID, Server: Server{
		Provider: heartbeatDirectProviderName,
		CloudID:  "direct-resource",
		Status:   "ready",
		Labels: map[string]string{
			"lease":             leaseID,
			"slug":              slug,
			"provider":          heartbeatDirectProviderName,
			"state":             "ready",
			"idle_timeout_secs": "1800",
		},
	}}
}
