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
	"sync"
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
	var coordinatorRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		coordinatorRequests.Add(1)
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
	snapshot, exists, set := ServerLeaseClaimSnapshot(backend.touches[0].Lease.Server)
	if coordinatorRequests.Load() != 1 || !set || !exists || snapshot.LeaseID != backend.lease.LeaseID {
		t.Fatalf("coordinator requests=%d snapshot=%#v exists=%t set=%t", coordinatorRequests.Load(), snapshot, exists, set)
	}
}

func TestHeartbeatRegisteredClaimReplacementPreventsProviderMutation(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	backend := &heartbeatDirectBackend{lease: heartbeatDirectTestLease("cbx_registered_race", "registered-race")}
	heartbeatDirectBackendForTest = backend
	t.Cleanup(func() { heartbeatDirectBackendForTest = nil })
	cfg := defaultConfig()
	cfg.Provider = heartbeatDirectProviderName
	if err := claimLeaseTargetForRepoConfig(backend.lease.LeaseID, serverSlug(backend.lease.Server), cfg, backend.lease.Server, SSHTarget{}, "/repo", 30*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	initial, err := readLeaseClaim(backend.lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	var coordinatorRequests, providerWrites atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		coordinatorRequests.Add(1)
		labels := cloneStringMap(initial.Labels)
		labels["owner"] = "replacement-owner"
		if _, err := updateLeaseClaimLabelsIfUnchanged(backend.lease.LeaseID, initial, labels); err != nil {
			t.Errorf("replace claim during coordinator heartbeat: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
			ID: backend.lease.LeaseID, Slug: "registered-race", Provider: heartbeatDirectProviderName, State: "active",
		}})
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	config := fmt.Sprintf("provider: %s\nbroker:\n  url: %s\n  mode: registered\n", heartbeatDirectProviderName, server.URL)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "user-token")
	backend.touchFn = func(req TouchRequest) (Server, error) {
		snapshot, exists, set := ServerLeaseClaimSnapshot(req.Lease.Server)
		if !set || !exists {
			return Server{}, errors.New("exact claim snapshot missing")
		}
		_, server, _, err := UpdateLeaseClaimTouchIfUnchangedAction(req.Lease.LeaseID, snapshot, time.Now(), req.IdleTimeoutOverride, func() (Server, SSHTarget, bool, error) {
			providerWrites.Add(1)
			return req.Lease.Server, req.Lease.SSH, true, nil
		})
		return server, err
	}
	err = (App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).heartbeat(context.Background(), []string{"--id", "registered-race"})
	if err == nil || !strings.Contains(err.Error(), "claim changed") || coordinatorRequests.Load() != 1 || providerWrites.Load() != 0 {
		t.Fatalf("heartbeat error=%v coordinator requests=%d successful provider writes=%d", err, coordinatorRequests.Load(), providerWrites.Load())
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
	touchFn    func(TouchRequest) (Server, error)
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
	if b.touchFn != nil {
		return b.touchFn(req)
	}
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
	snapshot, exists, set := ServerLeaseClaimSnapshot(backend.touches[0].Lease.Server)
	persisted, err := readLeaseClaim(backend.lease.LeaseID)
	if err != nil || !set || !exists || snapshot.Revision != persisted.Revision || backend.configures < 2 {
		t.Fatalf("snapshot=%#v exists=%t set=%t persisted=%#v configures=%d err=%v", snapshot, exists, set, persisted, backend.configures, err)
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

const (
	heartbeatDelegatedProviderName            = "heartbeat-delegated-test"
	heartbeatDelegatedUnsupportedProviderName = "heartbeat-delegated-unsupported-test"
)

// heartbeatDelegatedIdleTimeout is what the fake capability reports back by
// default. It is deliberately unlike any Crabbox config default so an assertion
// on the rendered value cannot pass by echoing config.
const heartbeatDelegatedIdleTimeout = 7 * time.Minute

// heartbeatDelegatedFixture holds the fake capability's observable state. The
// provider registry is process-wide, so this is guarded rather than left as
// plain package variables: a future t.Parallel() would otherwise race silently.
type heartbeatDelegatedFixture struct {
	mu          sync.Mutex
	requests    []LeaseHeartbeatRequest
	idleTimeout time.Duration
}

var heartbeatDelegated = &heartbeatDelegatedFixture{idleTimeout: heartbeatDelegatedIdleTimeout}

// arm sets what the capability reports for one test and restores the default.
func (f *heartbeatDelegatedFixture) arm(t *testing.T, idleTimeout time.Duration) {
	t.Helper()
	f.set(nil, idleTimeout)
	t.Cleanup(func() { f.set(nil, heartbeatDelegatedIdleTimeout) })
}

func (f *heartbeatDelegatedFixture) set(requests []LeaseHeartbeatRequest, idleTimeout time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = requests
	f.idleTimeout = idleTimeout
}

// record logs one call and returns the idle window to report for it.
func (f *heartbeatDelegatedFixture) record(req LeaseHeartbeatRequest) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	return f.idleTimeout
}

func (f *heartbeatDelegatedFixture) recorded() []LeaseHeartbeatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]LeaseHeartbeatRequest(nil), f.requests...)
}

func init() {
	RegisterProvider(heartbeatDelegatedProvider{})
	RegisterProvider(heartbeatDelegatedUnsupportedProvider{})
}

type heartbeatDelegatedProvider struct{}

func (heartbeatDelegatedProvider) Name() string      { return heartbeatDelegatedProviderName }
func (heartbeatDelegatedProvider) Aliases() []string { return nil }
func (heartbeatDelegatedProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        heartbeatDelegatedProviderName,
		Family:      "heartbeat-test",
		Kind:        ProviderKindDelegatedRun,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureLeaseHeartbeat},
		Coordinator: CoordinatorNever,
	}
}
func (heartbeatDelegatedProvider) RegisterFlags(*flag.FlagSet, Config) any { return noProviderFlags{} }
func (heartbeatDelegatedProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p heartbeatDelegatedProvider) Configure(Config, Runtime) (Backend, error) {
	return heartbeatDelegatedBackend{spec: p.Spec()}, nil
}

type heartbeatDelegatedBackend struct {
	spec ProviderSpec
}

func (b heartbeatDelegatedBackend) Spec() ProviderSpec { return b.spec }

func (b heartbeatDelegatedBackend) Heartbeat(_ context.Context, req LeaseHeartbeatRequest) (LeaseHeartbeatResult, error) {
	return LeaseHeartbeatResult{
		LeaseID:       req.ID,
		Slug:          "delegated-heartbeat",
		State:         "running",
		LastTouchedAt: time.Date(2026, 8, 16, 20, 0, 0, 0, time.UTC),
		IdleTimeout:   heartbeatDelegated.record(req),
	}, nil
}

// heartbeatDelegatedUnsupportedProvider is the same shape without the optional
// capability, so the negative direction stays pinned to today's behaviour.
type heartbeatDelegatedUnsupportedProvider struct{}

func (heartbeatDelegatedUnsupportedProvider) Name() string {
	return heartbeatDelegatedUnsupportedProviderName
}
func (heartbeatDelegatedUnsupportedProvider) Aliases() []string { return nil }
func (heartbeatDelegatedUnsupportedProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        heartbeatDelegatedUnsupportedProviderName,
		Family:      "heartbeat-test",
		Kind:        ProviderKindDelegatedRun,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Coordinator: CoordinatorNever,
	}
}
func (heartbeatDelegatedUnsupportedProvider) RegisterFlags(*flag.FlagSet, Config) any {
	return noProviderFlags{}
}
func (heartbeatDelegatedUnsupportedProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p heartbeatDelegatedUnsupportedProvider) Configure(Config, Runtime) (Backend, error) {
	return heartbeatUnsupportedBackend{spec: p.Spec()}, nil
}

type heartbeatUnsupportedBackend struct {
	spec ProviderSpec
}

func (b heartbeatUnsupportedBackend) Spec() ProviderSpec { return b.spec }

func TestHeartbeatDelegatedCapability(t *testing.T) {
	tests := []struct {
		name string
		// registeredBroker configures broker.mode=registered plus a
		// coordinator URL, the shape that used to disable the capability
		// wholesale.
		registeredBroker bool
		provider         string
		args             []string
		wantErrCode      int
		wantErr          string
	}{
		{
			name:     "capability keeps a delegated lease alive",
			provider: heartbeatDelegatedProviderName,
		},
		{
			// A CoordinatorNever provider can never hold a
			// coordinator-registered lease, so a team-wide registered broker
			// config must not disable the capability for it.
			name:             "registered broker does not disable a coordinator-never provider",
			registeredBroker: true,
			provider:         heartbeatDelegatedProviderName,
		},
		{
			name:        "provider without the capability still fails",
			provider:    heartbeatDelegatedUnsupportedProviderName,
			wantErrCode: 2,
			wantErr:     "provider=" + heartbeatDelegatedUnsupportedProviderName + " does not support lease heartbeat",
		},
		{
			// The delegated path reports the provider's idle window and has no
			// way to replace one, so the flag is refused instead of silently
			// doing nothing.
			name:        "idle timeout replacement is refused, not ignored",
			provider:    heartbeatDelegatedProviderName,
			args:        []string{"--idle-timeout", "20m"},
			wantErrCode: 2,
			wantErr:     "provider=" + heartbeatDelegatedProviderName + " does not support replacing the lease idle timeout while heartbeating",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
			if test.registeredBroker {
				t.Setenv("CRABBOX_COORDINATOR", "https://coordinator.example.test")
				t.Setenv("CRABBOX_COORDINATOR_MODE", string(BrokerModeRegistered))
				t.Setenv("CRABBOX_COORDINATOR_TOKEN", "test-token")
			}
			heartbeatDelegated.arm(t, heartbeatDelegatedIdleTimeout)

			args := append([]string{"--provider", test.provider, "--id", "cbx_delegated", "--json"}, test.args...)
			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).heartbeat(context.Background(), args)

			if test.wantErr != "" {
				var exitErr ExitError
				if !AsExitError(err, &exitErr) || exitErr.Code != test.wantErrCode || exitErr.Message != test.wantErr {
					t.Fatalf("error=%v, want exit %d %q", err, test.wantErrCode, test.wantErr)
				}
				if stdout.Len() != 0 {
					t.Fatalf("refused heartbeat wrote stdout=%q", stdout.String())
				}
				if calls := heartbeatDelegated.recorded(); len(calls) != 0 {
					t.Fatalf("refused heartbeat still reached the capability: %#v", calls)
				}
				return
			}

			if err != nil {
				t.Fatalf("heartbeat error=%v stderr=%q", err, stderr.String())
			}
			calls := heartbeatDelegated.recorded()
			if len(calls) != 1 {
				t.Fatalf("capability calls=%#v", calls)
			}
			if calls[0].ID != "cbx_delegated" {
				t.Fatalf("heartbeat request=%#v", calls[0])
			}
			var view leaseHeartbeatView
			if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if view.ID != "cbx_delegated" || view.Provider != heartbeatDelegatedProviderName || view.State != "running" ||
				view.LastTouchedAt != "2026-08-16T20:00:00Z" {
				t.Fatalf("heartbeat view=%#v", view)
			}
			// The rendered idle window is the provider's reported value, not
			// Crabbox's configured default.
			if view.IdleTimeout != heartbeatDelegatedIdleTimeout.String() {
				t.Fatalf("heartbeat view idleTimeout=%q, want the provider-reported %q", view.IdleTimeout, heartbeatDelegatedIdleTimeout)
			}
			if view.IdleTimeout == defaultConfig().IdleTimeout.String() {
				t.Fatalf("fixture is tautological: provider value equals the config default %q", view.IdleTimeout)
			}
			// LeaseHeartbeatResult carries no deadline field at all, so the
			// rendered view structurally cannot claim an expiry here.
			if view.ExpiresAt != "" {
				t.Fatalf("heartbeat view invented an absolute deadline: %#v", view)
			}
		})
	}
}

// TestHeartbeatDelegatedOmitsUnreportedIdleTimeout pins the other half of the
// honest-reporting rule: a provider that reports no idle window renders none,
// rather than falling back to the local config default.
func TestHeartbeatDelegatedOmitsUnreportedIdleTimeout(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	heartbeatDelegated.arm(t, 0)

	var stdout, stderr bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: &stderr}).heartbeat(context.Background(), []string{
		"--provider", heartbeatDelegatedProviderName, "--id", "cbx_delegated", "--json",
	}); err != nil {
		t.Fatalf("heartbeat error=%v stderr=%q", err, stderr.String())
	}
	var view leaseHeartbeatView
	if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.IdleTimeout != "" {
		t.Fatalf("unreported idle window rendered as %q", view.IdleTimeout)
	}

	// The text view marks it absent rather than printing a number.
	stdout.Reset()
	if err := (App{Stdout: &stdout, Stderr: &stderr}).heartbeat(context.Background(), []string{
		"--provider", heartbeatDelegatedProviderName, "--id", "cbx_delegated",
	}); err != nil {
		t.Fatalf("heartbeat error=%v stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "idle_timeout=-") {
		t.Fatalf("text view=%q, want idle_timeout=-", stdout.String())
	}
}
