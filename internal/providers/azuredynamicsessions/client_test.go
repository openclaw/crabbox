package azuredynamicsessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAzureDynamicSessionsEndpointRequiresPoolManagementEndpoint(t *testing.T) {
	cfg := Config{}
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
	RegisterAzureDynamicSessionsProviderFlags(fs, Config{})
	if fs.Lookup("azure-dynamic-sessions-pool") != nil {
		t.Fatal("azure-dynamic-sessions-pool should not be registered")
	}

	cfg := Config{}
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
			cfg := Config{}
			cfg.AzureDynamicSessions.Endpoint = endpoint
			_, err := azureDynamicSessionsEndpoint(cfg)
			if err == nil {
				t.Fatal("endpoint should be rejected")
			}
		})
	}
}

func TestAzureDynamicSessionsEndpointAllowsLoopbackHTTPForLocalRunner(t *testing.T) {
	cfg := Config{}
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
	runner := &recordingRunner{result: LocalCommandResult{Stdout: "token\n"}}
	cfg := Config{AzureTenant: "tenant-1", AzureSubscription: "sub-1"}
	token, err := azureDynamicSessionsAccessToken(context.Background(), cfg, Runtime{Exec: runner})
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
	runner := &recordingRunner{result: LocalCommandResult{Stdout: "az-token\n"}}

	token, err := azureDynamicSessionsAccessToken(context.Background(), Config{}, Runtime{Exec: runner})
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
	calls  []LocalCommandRequest
	result LocalCommandResult
}

func (r *recordingRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	return r.result, nil
}
