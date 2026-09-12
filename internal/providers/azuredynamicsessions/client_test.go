package azuredynamicsessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestAzureDynamicSessionsFlagRouteAndDeferredPoolContract(t *testing.T) {
	p := Provider{}
	cfg := core.Config{}
	if err := p.RouteConfig(&cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.AzureBackend != core.AzureBackendDynamicSessions {
		t.Fatal("Azure backend route changed")
	}
	for _, target := range []string{"", core.TargetLinux, "darwin"} {
		cfg := core.Config{TargetOS: target}
		b, err := p.Configure(cfg, core.Runtime{})
		if target == "darwin" {
			if err == nil || err.Error() != "azure-dynamic-sessions supports target=linux only" {
				t.Fatalf("target check=%v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		got := b.(*azureDynamicSessionsBackend).cfg
		if got.Provider != providerName || got.TargetOS != core.TargetLinux {
			t.Fatal("Configure canonical selection changed")
		}
	}
	cfg = core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterAzureDynamicSessionsProviderFlags(fs, cfg)
	if fs.Lookup("azure-dynamic-sessions-pool") != nil {
		t.Fatal("legacy Pool acquired a flag")
	}
	cfg.AzureDynamicSessions = core.AzureDynamicSessionsConfig{Endpoint: "https://example.invalid/pool", Pool: "legacy", APIVersion: "version", Workdir: "/workspace/app", TimeoutSecs: 12}
	before := cfg
	if err := ApplyAzureDynamicSessionsProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, before) {
		t.Fatal("unvisited flags changed config")
	}
	if err := fs.Parse([]string{"--azure-dynamic-sessions-endpoint=https://example.invalid/pool", "--azure-dynamic-sessions-api-version=version", "--azure-dynamic-sessions-workdir=/workspace/app", "--azure-dynamic-sessions-timeout-secs=12"}); err != nil {
		t.Fatal(err)
	}
	cfg.AzureDynamicSessions = core.AzureDynamicSessionsConfig{Pool: "legacy"}
	if err := ApplyAzureDynamicSessionsProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	core.RecordProviderFlagInputs(&before, true, "azure-dynamic-sessions")
	if !reflect.DeepEqual(cfg, before) {
		t.Fatal("positive flags changed fields or source phase")
	}
	if err := fs.Parse([]string{"--azure-dynamic-sessions-endpoint=", "--azure-dynamic-sessions-api-version=", "--azure-dynamic-sessions-workdir=", "--azure-dynamic-sessions-timeout-secs=-1"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyAzureDynamicSessionsProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal("flag application performed deferred validation")
	}
	before.AzureDynamicSessions = core.AzureDynamicSessionsConfig{Pool: "legacy", TimeoutSecs: -1}
	if !reflect.DeepEqual(cfg, before) {
		t.Fatal("explicit fields or central marker phase changed")
	}
	if err := fs.Parse([]string{"--azure-dynamic-sessions-timeout-secs=0"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyAzureDynamicSessionsProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.AzureDynamicSessions.TimeoutSecs != 0 {
		t.Fatal("explicit flag zero ignored")
	}
	if _, err := azureDynamicSessionsEndpoint(cfg); err == nil || err.Error() != "azureDynamicSessions.pool is not supported; set azureDynamicSessions.endpoint to the custom container poolManagementEndpoint" {
		t.Fatalf("legacy Pool later rejection=%v", err)
	}
	for _, name := range []string{providerName, "Azure-Dynamic-Sessions", " azure-dynamic-sessions "} {
		cfg := core.Config{Provider: name}
		fs := flag.NewFlagSet("guard", flag.ContinueOnError)
		fs.String("class", "", "")
		fs.String("type", "", "")
		values := RegisterAzureDynamicSessionsProviderFlags(fs, cfg)
		for _, args := range [][]string{{"--type=vm"}, {"--type=vm", "--class=large"}} {
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			for _, v := range []any{nil, struct{}{}, values} {
				err := ApplyAzureDynamicSessionsProviderFlags(&cfg, fs, v)
				if name != providerName {
					if err != nil {
						t.Fatal("provider guard gained normalization")
					}
					continue
				}
				want := "--type"
				if len(args) == 2 {
					want = "--class"
				}
				if err == nil || err.Error() != want+" is not supported for provider=azure-dynamic-sessions; choose pool sizing in Azure" {
					t.Fatalf("guard=%v", err)
				}
			}
		}
	}
}

func TestAzureDynamicSessionsDefaultConsumersWithMockedAuth(t *testing.T) {
	t.Setenv(tokenEnvName, "")
	for _, raw := range []string{"", "  ", " custom-version "} {
		cfg := core.Config{AzureDynamicSessions: core.AzureDynamicSessionsConfig{Endpoint: "http://127.0.0.1:8787", APIVersion: raw, Workdir: "  "}}
		runner := &recordingRunner{result: core.LocalCommandResult{Stdout: "inert-mocked-auth\n"}}
		api, err := newAzureDynamicSessionsClient(context.Background(), cfg, core.Runtime{Exec: runner, HTTP: &http.Client{}})
		if err != nil {
			t.Fatal(err)
		}
		want := "2025-02-02-preview"
		if strings.TrimSpace(raw) != "" {
			want = "custom-version"
		}
		if api.(*azureDynamicSessionsClient).managementAPIVersion != want {
			t.Fatal("API version default changed")
		}
		if len(runner.calls) != 1 || runner.calls[0].Name != "az" {
			t.Fatal("authentication did not stay in recording runtime")
		}
		if got, err := azureDynamicSessionsWorkspace(cfg); err != nil || got != "/workspace/crabbox" {
			t.Fatalf("workspace default=%q error=%v", got, err)
		}
	}
	for _, tc := range []struct {
		configured int
		ttl        time.Duration
		want       int
	}{{0, 0, 1800}, {-1, 0, 1800}, {0, -time.Second, 1800}, {0, 1500 * time.Millisecond, 2}, {-1, 42 * time.Second, 42}, {7, 42 * time.Second, 7}} {
		cfg := core.Config{TTL: tc.ttl, AzureDynamicSessions: core.AzureDynamicSessionsConfig{TimeoutSecs: tc.configured}}
		if got := azureDynamicSessionsTimeoutSeconds(cfg); got != tc.want {
			t.Fatalf("timeout configured=%d ttl=%s got=%d want=%d", tc.configured, tc.ttl, got, tc.want)
		}
	}
	cfg := core.Config{AzureDynamicSessions: core.AzureDynamicSessionsConfig{Workdir: " /workspace/custom/ "}}
	if got, err := azureDynamicSessionsWorkspace(cfg); err != nil || got != "/workspace/custom" {
		t.Fatalf("custom workspace=%q error=%v", got, err)
	}
	defaults := core.BaseConfig().AzureDynamicSessions
	if defaults.APIVersion != "2025-02-02-preview" || defaults.Workdir != "/workspace/crabbox" || defaults.TimeoutSecs != 1800 {
		t.Fatal("compiled defaults changed")
	}
	runner := &recordingRunner{result: core.LocalCommandResult{Stdout: "inert"}}
	_, err := newAzureDynamicSessionsClient(context.Background(), core.Config{AzureDynamicSessions: core.AzureDynamicSessionsConfig{Pool: "legacy"}}, core.Runtime{Exec: runner, HTTP: &http.Client{}})
	if err == nil || len(runner.calls) != 0 {
		t.Fatal("legacy Pool validation must precede mocked authentication")
	}
}

func TestAzureDynamicSessionsFallbackBoundsControlAndPreservesExecStream(t *testing.T) {
	const controlTimeout = 30 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.management/getSession":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"identifier":`)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/v1/exec":
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"type":"heartbeat"}`+"\n")
			w.(http.Flusher).Flush()
			time.Sleep(3 * controlTimeout)
			_, _ = io.WriteString(w, `{"type":"complete","exitCode":0}`+"\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	control, data := shared.ControlAndDataHTTPClients(nil, controlTimeout)
	client := &azureDynamicSessionsClient{
		endpoint:             server.URL,
		managementAPIVersion: "2025-02-02-preview",
		token:                "test-token",
		httpClient:           control,
		dataHTTPClient:       data,
	}
	started := time.Now()
	_, err := client.GetSession(context.Background(), "azds-test")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetSession error=%v, want whole-request deadline", err)
	}
	controlElapsed := time.Since(started)
	if controlElapsed >= time.Second {
		t.Fatalf("stalled control response bounded after %s, want under 1s", controlElapsed)
	}

	started = time.Now()
	code, err := client.ExecStream(context.Background(), "azds-test", azureDynamicSessionsExecRequest{Command: "true"}, io.Discard, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("ExecStream code=%d err=%v", code, err)
	}
	dataElapsed := time.Since(started)
	if dataElapsed <= controlTimeout {
		t.Fatalf("exec stream completed in %s, want beyond %s", dataElapsed, controlTimeout)
	}
	t.Logf("Azure Dynamic Sessions control body bounded in %s; exec stream completed in %s beyond %s control deadline", controlElapsed.Round(time.Millisecond), dataElapsed.Round(time.Millisecond), controlTimeout)
}

func TestAzureDynamicSessionsInjectedHTTPClientIsPreservedForBothPlanes(t *testing.T) {
	t.Setenv(tokenEnvName, "test-token")
	injected := &http.Client{Timeout: 17 * time.Second}
	api, err := newAzureDynamicSessionsClient(context.Background(), core.Config{AzureDynamicSessions: core.AzureDynamicSessionsConfig{
		Endpoint: "http://127.0.0.1:8787",
	}}, core.Runtime{HTTP: injected})
	if err != nil {
		t.Fatal(err)
	}
	client := api.(*azureDynamicSessionsClient)
	if client.httpClient != injected || client.dataHTTPClient != injected {
		t.Fatalf("clients=control:%p data:%p, want injected %p", client.httpClient, client.dataHTTPClient, injected)
	}
}

func TestAzureDynamicSessionsEndpointRequiresPoolManagementEndpoint(t *testing.T) {
	cfg := core.Config{}
	if _, err := azureDynamicSessionsEndpoint(cfg); err == nil {
		t.Fatal("endpoint should be required")
	}
	cfg.AzureDynamicSessions.Endpoint = "https://pool.env.eastus.azurecontainerapps.io/"
	got, err := azureDynamicSessionsEndpoint(cfg)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	want := "https://pool.env.eastus.azurecontainerapps.io"
	if got != want {
		t.Fatalf("endpoint = %q, want %q", got, want)
	}
}

func TestAzureDynamicSessionsPoolSelectorIsUnsupported(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterAzureDynamicSessionsProviderFlags(fs, core.Config{})
	if fs.Lookup("azure-dynamic-sessions-pool") != nil {
		t.Fatal("azure-dynamic-sessions-pool should not be registered")
	}

	cfg := core.Config{}
	cfg.AzureDynamicSessions.Endpoint = "https://pool.env.eastus.azurecontainerapps.io"
	cfg.AzureDynamicSessions.Pool = "pool"
	_, err := azureDynamicSessionsEndpoint(cfg)
	if err == nil || !strings.Contains(err.Error(), "azureDynamicSessions.pool is not supported") {
		t.Fatalf("err = %v, want unsupported pool config", err)
	}
}

func TestAzureDynamicSessionsEndpointRejectsUnsafeTokenDestinations(t *testing.T) {
	for _, endpoint := range []string{
		"http://pool.env.eastus.azurecontainerapps.io",
		"https://user:pass@pool.env.eastus.azurecontainerapps.io",
		"https://pool.env.eastus.azurecontainerapps.io?debug=true",
		"https://pool.env.eastus.azurecontainerapps.io#fragment",
		"https://pool.env.eastus.azurecontainerapps.io.evil.example",
		"https://evil.example",
		"pool.env.eastus.azurecontainerapps.io",
	} {
		t.Run(endpoint, func(t *testing.T) {
			cfg := core.Config{}
			cfg.AzureDynamicSessions.Endpoint = endpoint
			_, err := azureDynamicSessionsEndpoint(cfg)
			if err == nil {
				t.Fatal("endpoint should be rejected")
			}
		})
	}
}

func TestAzureDynamicSessionsEndpointAllowsLoopbackHTTPForLocalRunner(t *testing.T) {
	cfg := core.Config{}
	cfg.AzureDynamicSessions.Endpoint = "http://127.0.0.1:8787/"
	got, err := azureDynamicSessionsEndpoint(cfg)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if got != "http://127.0.0.1:8787" {
		t.Fatalf("endpoint = %q", got)
	}
}

func TestAzureDynamicSessionsAccessTokenUsesDynamicsessionsAudience(t *testing.T) {
	t.Setenv(tokenEnvName, "")
	runner := &recordingRunner{result: core.LocalCommandResult{Stdout: "token\n"}}
	cfg := core.Config{AzureTenant: "tenant-1", AzureSubscription: "sub-1"}
	token, err := azureDynamicSessionsAccessToken(context.Background(), cfg, core.Runtime{Exec: runner})
	if err != nil {
		t.Fatalf("access token: %v", err)
	}
	if token != "token" {
		t.Fatalf("token = %q, want token", token)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(runner.calls))
	}
	args := strings.Join(runner.calls[0].Args, " ")
	if !strings.Contains(args, "--resource "+dynamicSessionsAudience) {
		t.Fatalf("az args = %q, want dynamicsessions audience", args)
	}
	if !strings.Contains(args, "--tenant tenant-1") || !strings.Contains(args, "--subscription sub-1") {
		t.Fatalf("az args = %q, want tenant and subscription", args)
	}
}

func TestAzureDynamicSessionsAccessTokenPrefersEnvironmentToken(t *testing.T) {
	t.Setenv(tokenEnvName, " env-token ")
	runner := &recordingRunner{result: core.LocalCommandResult{Stdout: "az-token\n"}}

	token, err := azureDynamicSessionsAccessToken(context.Background(), core.Config{}, core.Runtime{Exec: runner})
	if err != nil {
		t.Fatalf("access token: %v", err)
	}
	if token != "env-token" {
		t.Fatalf("token = %q, want env-token", token)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("az was called despite env token: %#v", runner.calls)
	}
}

func TestAzureDynamicSessionsClientUsesCustomContainerEndpoints(t *testing.T) {
	var sawHealth, sawUpload, sawExec, sawGet, sawList, sawDelete bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/health":
			sawHealth = true
			if r.Method != http.MethodGet || r.URL.Query().Get("identifier") != "azds-test" {
				t.Fatalf("health method=%s query=%s", r.Method, r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/files":
			sawUpload = true
			if r.Method != http.MethodPost || r.URL.Query().Get("identifier") != "azds-test" || r.URL.Query().Get("path") != "/tmp/archive.tgz" {
				t.Fatalf("upload query = %s", r.URL.RawQuery)
			}
			uploaded, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read upload: %v", err)
			}
			if string(uploaded) != "archive" {
				t.Fatalf("upload body = %q", uploaded)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/v1/exec":
			sawExec = true
			if r.Method != http.MethodPost || r.URL.Query().Get("identifier") != "azds-test" {
				t.Fatalf("exec method=%s query=%s", r.Method, r.URL.RawQuery)
			}
			var body azureDynamicSessionsExecRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode exec body: %v", err)
			}
			if body.Command != "echo ok" || body.Cwd != "/workspace/crabbox" {
				t.Fatalf("exec body = %#v", body)
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte("{\"type\":\"stdout\",\"data\":\"ok\\n\"}\n{\"type\":\"complete\",\"exitCode\":7}\n"))
		case "/.management/getSession":
			sawGet = true
			if r.Method != http.MethodPost || r.URL.Query().Get("api-version") != "2025-02-02-preview" || r.URL.Query().Get("identifier") != "azds-test" {
				t.Fatalf("get query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"identifier":"azds-test","expiresAt":"2026-01-01T00:00:00Z"}`))
		case "/.management/listSessions":
			sawList = true
			if r.Method != http.MethodPost || r.URL.Query().Get("api-version") != "2025-02-02-preview" || r.URL.Query().Get("skip") != "0" {
				t.Fatalf("list query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"value":[{"identifier":"azds-test"}]}`))
		case "/.management/stopSession":
			sawDelete = true
			if r.Method != http.MethodPost || r.URL.Query().Get("api-version") != "2025-02-02-preview" || r.URL.Query().Get("identifier") != "azds-test" {
				t.Fatalf("delete query = %s", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := &azureDynamicSessionsClient{
		endpoint:             server.URL,
		managementAPIVersion: "2025-02-02-preview",
		token:                "test-token",
		httpClient:           server.Client(),
	}
	if err := client.CheckRunner(context.Background(), "azds-test"); err != nil {
		t.Fatalf("check runner: %v", err)
	}
	dir := t.TempDir()
	archive := filepath.Join(dir, "crabbox-azds-sync-test.tgz")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	if err := client.UploadFile(context.Background(), "azds-test", archive, "/tmp/archive.tgz"); err != nil {
		t.Fatalf("upload: %v", err)
	}
	var stdout bytes.Buffer
	exitCode, err := client.ExecStream(context.Background(), "azds-test", azureDynamicSessionsExecRequest{
		Command: "echo ok",
		Cwd:     "/workspace/crabbox",
	}, &stdout, nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if exitCode != 7 || stdout.String() != "ok\n" {
		t.Fatalf("exec exit=%d stdout=%q", exitCode, stdout.String())
	}
	if _, err := client.GetSession(context.Background(), "azds-test"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := client.ListSessions(context.Background()); err != nil {
		t.Fatalf("list: %v", err)
	}
	if err := client.DeleteSession(context.Background(), "azds-test"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !sawHealth || !sawUpload || !sawExec || !sawGet || !sawList || !sawDelete {
		t.Fatalf("saw health=%t upload=%t exec=%t get=%t list=%t delete=%t", sawHealth, sawUpload, sawExec, sawGet, sawList, sawDelete)
	}
}

func TestAzureDynamicSessionsListSessionsFollowsNextLink(t *testing.T) {
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.String())
		switch r.URL.Query().Get("skip") {
		case "0":
			_, _ = w.Write([]byte(`{"value":[{"identifier":"azds-one"}],"nextLink":"/.management/listSessions?skip=1"}`))
		case "1":
			_, _ = w.Write([]byte(`{"sessions":[{"properties":{"identifier":"azds-two","status":"Running"}}]}`))
		default:
			t.Fatalf("unexpected list query = %s", r.URL.RawQuery)
		}
	}))
	defer server.Close()

	client := &azureDynamicSessionsClient{
		endpoint:             server.URL,
		managementAPIVersion: "2025-02-02-preview",
		token:                "test-token",
		httpClient:           server.Client(),
	}
	sessions, err := client.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].Identifier != "azds-one" || sessions[1].Identifier != "azds-two" || sessions[1].Status != "Running" {
		t.Fatalf("sessions = %#v", sessions)
	}
	if len(requests) != 2 || !strings.Contains(requests[1], "api-version=2025-02-02-preview") {
		t.Fatalf("requests = %#v, want paginated api-version", requests)
	}
}

func TestAzureDynamicSessionsListSessionsRejectsCrossOriginNextLink(t *testing.T) {
	attackerCalled := false
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerCalled = true
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization leaked to nextLink host: %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer attacker.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"value":[{"identifier":"azds-one"}],"nextLink":` + strconv.Quote(attacker.URL+"/.management/listSessions?skip=1") + `}`))
	}))
	defer server.Close()

	client := &azureDynamicSessionsClient{
		endpoint:             server.URL,
		managementAPIVersion: "2025-02-02-preview",
		token:                "test-token",
		httpClient:           server.Client(),
	}
	_, err := client.ListSessions(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nextLink points outside configured endpoint origin") {
		t.Fatalf("err = %v, want cross-origin nextLink rejection", err)
	}
	if attackerCalled {
		t.Fatal("cross-origin nextLink was requested")
	}
}

func TestAzureDynamicSessionsClientRejectsCrossOriginRedirects(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization=%q", got)
		}
		http.Redirect(w, r, target.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	archive := filepath.Join(t.TempDir(), "archive.tgz")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &azureDynamicSessionsClient{
		endpoint:             source.URL,
		managementAPIVersion: "2025-02-02-preview",
		token:                "test-token",
		httpClient:           source.Client(),
	}
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "exec", run: func() error {
			_, err := client.ExecStream(context.Background(), "azds-test", azureDynamicSessionsExecRequest{
				Command: "env",
				Env:     map[string]string{"MARKER": "fixture-value"},
			}, io.Discard, io.Discard)
			return err
		}},
		{name: "upload", run: func() error {
			return client.UploadFile(context.Background(), "azds-test", archive, "/tmp/archive.tgz")
		}},
		{name: "json", run: func() error {
			return client.DeleteSession(context.Background(), "azds-test")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run()
			if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
				t.Fatalf("error=%v, want cross-origin redirect rejection", err)
			}
		})
	}
	if got := redirected.Load(); got != 0 {
		t.Fatalf("redirect target received %d requests", got)
	}
}

func TestAzureDynamicSessionsClientAllowsSameOriginRedirect(t *testing.T) {
	var redirected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization=%q", got)
		}
		if r.URL.Path == "/health" {
			http.Redirect(w, r, "/health-ok", http.StatusTemporaryRedirect)
			return
		}
		redirected.Add(1)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	client := &azureDynamicSessionsClient{
		endpoint:   server.URL,
		token:      "test-token",
		httpClient: server.Client(),
	}
	if err := client.CheckRunner(context.Background(), "azds-test"); err != nil {
		t.Fatal(err)
	}
	if got := redirected.Load(); got != 1 {
		t.Fatalf("same-origin target received %d requests", got)
	}
}

func TestAzureDynamicSessionsExecStreamRejectsIncompleteStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte("{\"type\":\"stdout\",\"data\":\"partial\"}\n"))
	}))
	defer server.Close()

	client := &azureDynamicSessionsClient{
		endpoint:   server.URL,
		token:      "test-token",
		httpClient: server.Client(),
	}
	if _, err := client.ExecStream(context.Background(), "azds-test", azureDynamicSessionsExecRequest{Command: "echo partial"}, nil, nil); err == nil || !strings.Contains(err.Error(), "ended before completion") {
		t.Fatalf("err = %v, want incomplete stream", err)
	}
}

func TestAzureDynamicSessionsClientRedactsReflectedCredential(t *testing.T) {
	const secret = "azure-session-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Bearer `+secret+` quota exceeded"}`)
	}))
	defer server.Close()
	client := &azureDynamicSessionsClient{endpoint: server.URL, token: secret, httpClient: server.Client()}

	for name, call := range map[string]func() error{
		"JSON": func() error { return client.CheckRunner(context.Background(), "azds-test") },
		"stream": func() error {
			_, err := client.ExecStream(context.Background(), "azds-test", azureDynamicSessionsExecRequest{Command: "true"}, nil, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "quota exceeded") {
				t.Fatalf("error=%v, want redacted useful provider error", err)
			}
		})
	}
}

func TestAzureDynamicSessionsClientRedactsStreamErrorCredential(t *testing.T) {
	const secret = "azure-stream-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"type":"error","error":"Bearer `+secret+` quota exceeded"}`+"\n")
	}))
	defer server.Close()
	client := &azureDynamicSessionsClient{endpoint: server.URL, token: secret, httpClient: server.Client()}
	_, err := client.ExecStream(context.Background(), "azds-test", azureDynamicSessionsExecRequest{Command: "true"}, nil, nil)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("ExecStream error=%v, want redacted useful stream error", err)
	}
}

func TestAzureDynamicSessionsExecStreamReturnsWriterErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		event   string
		stdout  io.Writer
		stderr  io.Writer
		wantErr string
	}{
		{
			name:    "stdout",
			event:   `{"type":"stdout","data":"out"}`,
			stdout:  errWriter("stdout closed"),
			wantErr: "write azure-dynamic-sessions stdout: stdout closed",
		},
		{
			name:    "stderr",
			event:   `{"type":"stderr","data":"err"}`,
			stderr:  errWriter("stderr closed"),
			wantErr: "write azure-dynamic-sessions stderr: stderr closed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = w.Write([]byte(tc.event + "\n"))
				_, _ = w.Write([]byte(`{"type":"complete","exitCode":0}` + "\n"))
			}))
			defer server.Close()

			client := &azureDynamicSessionsClient{
				endpoint:   server.URL,
				token:      "test-token",
				httpClient: server.Client(),
			}
			exitCode, err := client.ExecStream(context.Background(), "azds-test", azureDynamicSessionsExecRequest{Command: "echo"}, tc.stdout, tc.stderr)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("exit=%d err=%v, want %q", exitCode, err, tc.wantErr)
			}
		})
	}
}

type errWriter string

func (w errWriter) Write([]byte) (int, error) {
	return 0, errors.New(string(w))
}

type recordingRunner struct {
	calls  []core.LocalCommandRequest
	result core.LocalCommandResult
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	return r.result, nil
}

type azureDynamicSessionsStreamEOFTransport struct {
	body io.ReadCloser
}

func (t azureDynamicSessionsStreamEOFTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/x-ndjson"}}, Body: t.body, Request: req}, nil
}

type azureDynamicSessionsCancelingEOFBody struct {
	data   string
	cancel context.CancelFunc
}

func (b *azureDynamicSessionsCancelingEOFBody) Read(p []byte) (int, error) {
	n := copy(p, b.data)
	b.data = b.data[n:]
	if b.data == "" {
		b.cancel()
		return n, io.EOF
	}
	return n, nil
}
func (*azureDynamicSessionsCancelingEOFBody) Close() error { return nil }

func TestAzureDynamicSessionsStreamCancellationAtCleanEOF(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			data := ""
			if complete {
				data = "{\"type\":\"complete\",\"exitCode\":7}\n"
			}
			client := &azureDynamicSessionsClient{endpoint: "http://127.0.0.1", httpClient: &http.Client{Transport: azureDynamicSessionsStreamEOFTransport{body: &azureDynamicSessionsCancelingEOFBody{data: data, cancel: cancel}}}}
			code, err := client.ExecStream(ctx, "fixture", azureDynamicSessionsExecRequest{Command: "true"}, io.Discard, io.Discard)
			if complete {
				if err != nil || code != 7 {
					t.Fatalf("accepted completion changed: code=%d err=%v", code, err)
				}
			} else if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost at clean EOF: %v", err)
			}
		})
	}
}

func TestJSONRequestAdoptionEnvelope(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "capture")
	var typedNil *struct{ Value string }
	const base = "https://api.example.test/base"
	sentinel := errors.New("synthetic captured transport stop")
	for _, tc := range []struct {
		name        string
		body        any
		want        string
		query, fail bool
	}{
		{name: "nil"},
		{name: "typed nil", body: typedNil, want: "null\n"},
		{name: "JSON bytes", body: map[string]string{"message": "<&>"}, want: "{\"message\":\"\\u003c\\u0026\\u003e\"}\n"},
		{name: "query without body", query: true},
		{name: "transport error", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			endpoint := "/records"
			if tc.query {
				endpoint += "?limit=2&prefix=two+words"
			}

			headers := http.Header{"Authorization": []string{"Bearer synthetic-token"}}
			headers.Set("Accept", "application/json")
			headers.Set("User-Agent", "crabbox/azure-dynamic-sessions")
			if tc.body != nil {
				headers.Set("Content-Type", "application/json")
			}
			transport := &http.Client{Transport: testutil.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				testutil.RequireRequestEnvelope(t, req, ctx, http.MethodPost, base+endpoint, tc.want, headers)
				if tc.fail {
					return nil, sentinel
				}
				return &http.Response{StatusCode: 204, Header: http.Header{"X-Capture": []string{"yes"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
			})}
			c := &azureDynamicSessionsClient{endpoint: base, token: "synthetic-token", httpClient: transport}
			var gotHeaders http.Header
			err := c.doJSONURL(ctx, http.MethodPost, base+endpoint, tc.body, nil)
			if tc.fail {
				if !errors.Is(err, sentinel) || gotHeaders != nil {
					t.Fatalf("error/headers=%v %v", err, gotHeaders)
				}
			} else if err != nil {
				t.Fatal(err)
			}

			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestJSONRequestAdoptionConcreteEnvelope(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("synthetic captured concrete request")
	calls := 0
	headers := http.Header{"Authorization": []string{"Bearer synthetic-token"}, "Content-Type": []string{"application/json"}}
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "crabbox/azure-dynamic-sessions")
	transport := &http.Client{Transport: testutil.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		testutil.RequireRequestEnvelope(t, req, ctx, http.MethodPost, "https://api.example.test/base/v1/exec?identifier=session+one", "{\"command\":\"\\u003c\\u0026\\u003e\",\"cwd\":\"/work\",\"env\":{\"A\":\"B\"},\"timeoutMs\":7}\n", headers)
		return nil, sentinel
	})}
	c := &azureDynamicSessionsClient{endpoint: "https://api.example.test/base", token: "synthetic-token", httpClient: transport}
	code, err := c.ExecStream(ctx, "session one", azureDynamicSessionsExecRequest{Command: "<&>", Cwd: "/work", Env: map[string]string{"A": "B"}, TimeoutMS: 7}, io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !errors.Is(err, sentinel) || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

type responseCloseObserver struct {
	io.Reader
	close func() error
}

func (b responseCloseObserver) Close() error { return b.close() }

func TestRawJSONResponseContract(t *testing.T) {
	readFailure := errors.New("synthetic response read failure")
	closeFailure := errors.New("synthetic ignored close failure")
	for _, tc := range []struct {
		name, data        string
		status            int
		nilOut, readError bool
		wantError         string
		wantValue         int
	}{
		{name: "nil output still reads invalid JSON", data: "not JSON", nilOut: true, wantValue: 99},
		{name: "raw empty", wantValue: 99},
		{name: "whitespace with output", data: " \t\n", wantError: "syntax", wantValue: 99},
		{name: "whitespace without output", data: " \t\n", nilOut: true, wantValue: 99},
		{name: "JSON object", data: `{"value":12}`, wantValue: 12},
		{name: "JSON null", data: "null", wantValue: 99},
		{name: "JSON trailing whitespace", data: "{\"value\":12}\n \t", wantValue: 12},
		{name: "trailing JSON value", data: `{"value":12}{"value":13}`, wantError: "syntax", wantValue: 99},
		{name: "wrong JSON field type", data: `{"value":"bad"}`, wantError: "type", wantValue: 99},
		{name: "status error", data: "  unavailable \n", status: 503, wantError: "status", wantValue: 99},
		{name: "status error with nil output", data: "  unavailable \n", status: 503, nilOut: true, wantError: "status", wantValue: 99},
		{name: "read error beats success", data: `{"value":12}`, readError: true, wantError: "read", wantValue: 99},
		{name: "read error beats status and nil output", data: "partial body", status: 503, nilOut: true, readError: true, wantError: "read", wantValue: 99},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.NewReader([]byte(tc.data))
			var reader io.Reader = payload
			if tc.readError {
				reader = io.MultiReader(payload, iotest.ErrReader(readFailure))
			}
			closed, calls := 0, 0
			responseHeaders := http.Header{"X-Trace": []string{"before-close", "second"}}
			body := responseCloseObserver{Reader: reader, close: func() error { closed++; responseHeaders["X-Trace"][0] = "after-close"; return closeFailure }}
			status := tc.status
			if status == 0 {
				status = 200
			}
			const base = "https://api.example.test"
			httpClient := &http.Client{Transport: testutil.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: status, Status: strconv.Itoa(status) + " Synthetic", Header: responseHeaders, Body: body, Request: req}, nil
			})}
			client := &azureDynamicSessionsClient{endpoint: base, token: "synthetic-token", httpClient: httpClient}
			out := struct {
				Value int `json:"value"`
			}{Value: 99}
			var destination any = &out
			if tc.nilOut {
				destination = nil
			}
			err := client.doJSONURL(context.Background(), http.MethodGet, base+"/records", nil, destination)
			if calls != 1 || closed != 1 || payload.Len() != 0 {
				t.Fatalf("calls=%d closes=%d unread=%d", calls, closed, payload.Len())
			}
			if out.Value != tc.wantValue {
				t.Fatalf("value=%d want%d", out.Value, tc.wantValue)
			}
			switch tc.wantError {
			case "":
				if err != nil {
					t.Fatal(err)
				}
			case "read":
				if err != readFailure {
					t.Fatalf("read error=%T %v want exact sentinel", err, err)
				}
			case "syntax":
				if _, ok := err.(*json.SyntaxError); !ok {
					t.Fatalf("decode error=%T %v want unwrapped SyntaxError", err, err)
				}
			case "type":
				if _, ok := err.(*json.UnmarshalTypeError); !ok {
					t.Fatalf("decode error=%T %v want unwrapped UnmarshalTypeError", err, err)
				}
			case "status":
				api, ok := err.(*azureDynamicSessionsAPIError)
				if !ok || api.StatusCode != 503 || api.Status != "503 Synthetic" || api.Body != "unavailable" {
					t.Fatalf("status error=%T %#v", err, err)
				}
			}

		})
	}
}
