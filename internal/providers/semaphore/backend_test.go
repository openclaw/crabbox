package semaphore

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type testClock struct{}

func (testClock) Now() time.Time { return time.Now() }

type semaphoreRoundTripFunc func(*http.Request) (*http.Response, error)

func (f semaphoreRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testConfig(host string) core.Config {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Semaphore.Host = host
	cfg.Semaphore.Token = "test-token"
	cfg.Semaphore.Project = "my-project"
	cfg.Semaphore.Machine = "f1-standard-2"
	cfg.Semaphore.OSImage = "ubuntu2204"
	cfg.Semaphore.IdleTimeout = "10m"
	return cfg
}

func testRuntime(httpClient *http.Client) core.Runtime {
	return core.Runtime{
		Stdout: io.Discard,
		Stderr: io.Discard,
		Clock:  testClock{},
		HTTP:   httpClient,
	}
}

func withWaitForRunningPollInterval(t *testing.T, interval time.Duration) {
	t.Helper()
	old := waitForRunningPollInterval
	waitForRunningPollInterval = interval
	t.Cleanup(func() { waitForRunningPollInterval = old })
}

// --- Provider registration ---

func TestProviderName(t *testing.T) {
	p := Provider{}
	if p.Name() != "semaphore" {
		t.Errorf("name = %q, want semaphore", p.Name())
	}
}

func TestProviderAliases(t *testing.T) {
	p := Provider{}
	aliases := p.Aliases()
	if len(aliases) != 1 || aliases[0] != "sem" {
		t.Errorf("aliases = %v, want [sem]", aliases)
	}
}

func TestProviderSpecIsSSHLease(t *testing.T) {
	p := Provider{}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindSSHLease {
		t.Errorf("kind = %q, want ssh-lease", spec.Kind)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Errorf("coordinator = %q, want never", spec.Coordinator)
	}
}

// --- Flag registration ---

func TestRegisterAndApplyFlags(t *testing.T) {
	cfg := core.BaseConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	values := registerFlags(fs, cfg)
	err := fs.Parse([]string{
		"--semaphore-host", "myorg.semaphoreci.com",
		"--semaphore-project", "my-app",
		"--semaphore-machine", "f1-standard-4",
		"--semaphore-os-image", "ubuntu2404",
		"--semaphore-idle-timeout", "15m",
	})
	if err != nil {
		t.Fatal(err)
	}

	applyFlagOverrides(&cfg, fs, values)

	if cfg.Semaphore.Host != "myorg.semaphoreci.com" {
		t.Errorf("host = %q", cfg.Semaphore.Host)
	}
	if cfg.Semaphore.Project != "my-app" {
		t.Errorf("project = %q", cfg.Semaphore.Project)
	}
	if cfg.Semaphore.Machine != "f1-standard-4" {
		t.Errorf("machine = %q", cfg.Semaphore.Machine)
	}
	if cfg.Semaphore.OSImage != "ubuntu2404" {
		t.Errorf("os_image = %q", cfg.Semaphore.OSImage)
	}
	if cfg.Semaphore.IdleTimeout != "15m" {
		t.Errorf("idle_timeout = %q", cfg.Semaphore.IdleTimeout)
	}
}

func TestFlagsNotSetLeavesDefaults(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Semaphore.Machine = "original"
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	values := registerFlags(fs, cfg)
	_ = fs.Parse([]string{}) // no flags

	applyFlagOverrides(&cfg, fs, values)

	if cfg.Semaphore.Machine != "original" {
		t.Errorf("machine changed to %q, should stay original", cfg.Semaphore.Machine)
	}
}

// --- Config helpers ---

func TestIdleTimeoutDefault(t *testing.T) {
	cfg := core.BaseConfig()
	d, err := idleTimeout(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if d != 30*time.Minute {
		t.Errorf("default idle timeout = %v, want 30m", d)
	}
}

func TestIdleTimeoutFromConfig(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Semaphore.IdleTimeout = "15m"
	d, err := idleTimeout(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if d != 15*time.Minute {
		t.Errorf("idle timeout = %v, want 15m", d)
	}
}

func TestIdleTimeoutRejectsInvalidConfig(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Semaphore.IdleTimeout = "later"
	if _, err := idleTimeout(cfg); err == nil {
		t.Fatal("expected invalid idle timeout error")
	}
}

func TestWithDefault(t *testing.T) {
	if withDefault("", "fallback") != "fallback" {
		t.Error("empty should use fallback")
	}
	if withDefault("value", "fallback") != "value" {
		t.Error("non-empty should use value")
	}
}

func TestNormalizeSemaphoreHost(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{name: "bare host", input: "myorg.semaphoreci.com", want: "myorg.semaphoreci.com"},
		{name: "trims whitespace", input: "  myorg.semaphoreci.com  ", want: "myorg.semaphoreci.com"},
		{name: "strips trailing slash", input: "myorg.semaphoreci.com/", want: "myorg.semaphoreci.com"},
		{name: "https url", input: "https://myorg.semaphoreci.com/", want: "myorg.semaphoreci.com"},
		{name: "host with port", input: "https://myorg.semaphoreci.com:443", want: "myorg.semaphoreci.com:443"},
		{name: "rejects plaintext url", input: "http://myorg.semaphoreci.com", wantErr: "not an API URL"},
		{name: "rejects explicit userinfo", input: "https://user:pass@attacker.example", wantErr: "not an API URL"},
		{name: "rejects bare userinfo confusion", input: "trusted.example@attacker.example", wantErr: "not an API URL"},
		{name: "rejects api path", input: "https://myorg.semaphoreci.com/api/v1alpha", wantErr: "not an API URL"},
		{name: "rejects query", input: "https://myorg.semaphoreci.com?token=secret", wantErr: "not an API URL"},
		{name: "rejects path without scheme", input: "myorg.semaphoreci.com/api/v1alpha", wantErr: "not an API URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeSemaphoreHost(tt.input)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("host=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestStripLeasePrefix(t *testing.T) {
	if stripLeasePrefix("sem_abc123") != "abc123" {
		t.Errorf("got %q", stripLeasePrefix("sem_abc123"))
	}
	if stripLeasePrefix("abc123") != "abc123" {
		t.Errorf("got %q", stripLeasePrefix("abc123"))
	}
}

// --- API client tests with httptest ---

func TestAPIClientRedactsTokenFromAllResponseErrors(t *testing.T) {
	const token = "semaphore-secret-token"
	tests := []struct {
		name string
		path string
		call func(*apiClient, string) error
	}{
		{
			name: "get with headers",
			path: "/get-with-headers",
			call: func(client *apiClient, path string) error {
				_, _, err := client.getWithHeaders(context.Background(), path)
				return err
			},
		},
		{name: "get", path: "/get", call: func(client *apiClient, path string) error {
			return client.get(context.Background(), path, nil)
		}},
		{name: "post", path: "/post", call: func(client *apiClient, path string) error {
			return client.post(context.Background(), path, map[string]string{"value": "test"}, nil)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &apiClient{
				host:  "semaphore.example.test",
				token: token,
				http: &http.Client{Transport: semaphoreRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					if got := req.Header.Get("Authorization"); got != "Token "+token {
						t.Fatalf("Authorization=%q", got)
					}
					return &http.Response{
						StatusCode: http.StatusUnauthorized,
						Body:       io.NopCloser(strings.NewReader("bad Authorization: Token " + token)),
						Header:     make(http.Header),
					}, nil
				})},
			}
			err := tc.call(client, tc.path)
			if err == nil {
				t.Fatal("API call returned nil")
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("API error leaked token: %v", err)
			}
			for _, want := range []string{tc.path, "401", "Token [redacted]"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("API error=%q, want %q", err, want)
				}
			}
		})
	}
	if got := redactSemaphoreSecrets("keep body", " "); got != "keep body" {
		t.Fatalf("empty secret changed response body: %q", got)
	}
}

func TestAPIClientTLSRedactsReflectedToken(t *testing.T) {
	const token = "semaphore-secret-token"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if got := req.Header.Get("Authorization"); got != "Token "+token {
			t.Fatalf("Authorization=%q", got)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "bad "+req.Header.Get("Authorization"))
	}))
	defer server.Close()

	client := &apiClient{
		host:  strings.TrimPrefix(server.URL, "https://"),
		token: token,
		http:  server.Client(),
	}
	err := client.get(context.Background(), "/api/v1alpha/projects", nil)
	if err == nil {
		t.Fatal("API call returned nil")
	}
	if strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "Token [redacted]") {
		t.Fatalf("TLS API error was not redacted: %v", err)
	}
}

func TestCreateJob(t *testing.T) {
	var receivedBody map[string]any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1alpha/projects" {
			json.NewEncoder(w).Encode([]map[string]any{
				{"metadata": map[string]string{"name": "my-project", "id": "proj-123"}},
			})
			return
		}
		if r.URL.Path == "/api/v1alpha/jobs" && r.Method == "POST" {
			json.NewDecoder(r.Body).Decode(&receivedBody)
			// Check auth header
			if auth := r.Header.Get("Authorization"); auth != "Token test-token" {
				t.Errorf("auth = %q", auth)
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]string{"id": "job-456"},
			})
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "https://")
	client := &apiClient{host: host, token: "test-token", http: server.Client()}

	jobID, err := client.CreateJob(context.Background(), "my-project", "f1-standard-2", "ubuntu2204", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if jobID != "job-456" {
		t.Errorf("jobID = %q, want job-456", jobID)
	}

	// Verify the job spec
	spec := receivedBody["spec"].(map[string]any)
	if spec["project_id"] != "proj-123" {
		t.Errorf("project_id = %v", spec["project_id"])
	}
	agent := spec["agent"].(map[string]any)
	machine := agent["machine"].(map[string]any)
	if machine["type"] != "f1-standard-2" {
		t.Errorf("machine type = %v", machine["type"])
	}
	if machine["os_image"] != "ubuntu2204" {
		t.Errorf("os_image = %v", machine["os_image"])
	}
}

func TestGetJobStatus(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]string{
				"name": "crabbox testbox",
			},
			"status": map[string]any{
				"state": "RUNNING",
				"agent": map[string]any{
					"ip": "1.2.3.4",
					"ports": []map[string]any{
						{"name": "ssh", "number": 40010},
					},
				},
			},
		})
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "https://")
	client := &apiClient{host: host, token: "tok", http: server.Client()}

	status, err := client.GetJobStatus(context.Background(), "job-123")
	if err != nil {
		t.Fatal(err)
	}
	if status.Name != "crabbox testbox" {
		t.Errorf("name = %q", status.Name)
	}
	if status.State != "RUNNING" {
		t.Errorf("state = %q", status.State)
	}
	if status.IP != "1.2.3.4" {
		t.Errorf("ip = %q", status.IP)
	}
	if status.SSHPort != 40010 {
		t.Errorf("port = %d", status.SSHPort)
	}
}

func TestWaitForRunningReturnsPermanentStatusErrors(t *testing.T) {
	withWaitForRunningPollInterval(t, 0)
	for _, tt := range []struct {
		name      string
		status    int
		body      string
		wantError string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: "bad token", wantError: "returned 401"},
		{name: "not found", status: http.StatusNotFound, body: "missing", wantError: "returned 404"},
		{name: "malformed json", status: http.StatusOK, body: "{", wantError: "unexpected end of JSON input"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "https://")
			client := &apiClient{host: host, token: "tok", http: server.Client()}

			_, _, err := client.WaitForRunning(context.Background(), "job-123", func() {})
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("err=%v, want %q", err, tt.wantError)
			}
			if calls != 1 {
				t.Fatalf("calls=%d want 1 permanent failure", calls)
			}
		})
	}
}

func TestWaitForRunningRetriesTransientStatusError(t *testing.T) {
	withWaitForRunningPollInterval(t, 0)
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "temporary")
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]string{"name": "crabbox testbox"},
			"status": map[string]any{
				"state": "RUNNING",
				"agent": map[string]any{
					"ip": "1.2.3.4",
					"ports": []map[string]any{
						{"name": "ssh", "number": 40010},
					},
				},
			},
		})
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "https://")
	client := &apiClient{host: host, token: "tok", http: server.Client()}

	ip, port, err := client.WaitForRunning(context.Background(), "job-123", func() {})
	if err != nil {
		t.Fatalf("WaitForRunning err=%v", err)
	}
	if ip != "1.2.3.4" || port != 40010 {
		t.Fatalf("target=%s:%d want 1.2.3.4:40010", ip, port)
	}
	if calls != 2 {
		t.Fatalf("calls=%d want 2", calls)
	}
}

func TestRetryableJobStatusErrorUsesPollingContext(t *testing.T) {
	if !retryableJobStatusError(context.Background(), context.DeadlineExceeded) {
		t.Fatal("per-request deadline should be retryable while polling context is active")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if retryableJobStatusError(ctx, context.DeadlineExceeded) {
		t.Fatal("deadline should not be retryable after polling context is canceled")
	}
	if retryableJobStatusError(context.Background(), &semaphoreAPIError{StatusCode: http.StatusUnauthorized}) {
		t.Fatal("401 should not be retryable")
	}
	if !retryableJobStatusError(context.Background(), &semaphoreAPIError{StatusCode: http.StatusTooManyRequests}) {
		t.Fatal("429 should be retryable")
	}
}

func TestGetSSHKey(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"key": "-----BEGIN RSA PRIVATE KEY-----\ntest\n-----END RSA PRIVATE KEY-----"})
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "https://")
	client := &apiClient{host: host, token: "tok", http: server.Client()}

	key, err := client.GetSSHKey(context.Background(), "job-123")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(key, "RSA PRIVATE KEY") {
		t.Errorf("key doesn't look like an SSH key: %q", key[:30])
	}
}

func TestResolveProjectID(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "SemaphoreCI v2.0 Client" {
			t.Errorf("user-agent = %q", got)
		}
		if r.URL.Path == "/api/v1alpha/projects/my-project" {
			w.WriteHeader(400) // some hosts don't support name lookup
			return
		}
		if r.URL.Path == "/api/v1alpha/projects" && r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `</api/v1alpha/projects?page=2>; rel="next"`)
			json.NewEncoder(w).Encode([]map[string]any{
				{"metadata": map[string]string{"name": "other", "id": "other-id"}},
			})
			return
		}
		if r.URL.Path == "/api/v1alpha/projects" && r.URL.Query().Get("page") == "2" {
			json.NewEncoder(w).Encode([]map[string]any{
				{"metadata": map[string]string{"name": "my-project", "id": "proj-abc"}},
			})
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "https://")
	client := &apiClient{host: host, token: "tok", http: server.Client()}

	id, err := client.resolveProjectID(context.Background(), "my-project")
	if err != nil {
		t.Fatal(err)
	}
	if id != "proj-abc" {
		t.Errorf("project id = %q, want proj-abc", id)
	}
}

func TestResolveProjectIDNotFound(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1alpha/projects/missing" {
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "https://")
	client := &apiClient{host: host, token: "tok", http: server.Client()}

	_, err := client.resolveProjectID(context.Background(), "missing")
	if err == nil {
		t.Error("expected error for missing project")
	}
}

func TestConfigureRequiresHostAndToken(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	// No host or token
	_, err := newBackend(Provider{}.Spec(), cfg, testRuntime(nil))
	if err == nil {
		t.Error("expected error when host/token missing")
	}
}

func TestConfigureNormalizesSemaphoreHost(t *testing.T) {
	cfg := testConfig("https://semaphore.example.test/")
	backend, err := newBackend(Provider{}.Spec(), cfg, testRuntime(nil))
	if err != nil {
		t.Fatal(err)
	}
	got := backend.(*semaphoreBackend).client.host
	if got != "semaphore.example.test" {
		t.Fatalf("host=%q, want semaphore.example.test", got)
	}
}

func TestStopJob(t *testing.T) {
	stopped := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/stop") {
			stopped = true
			w.WriteHeader(http.StatusNoContent)
			w.Write([]byte("{}"))
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "https://")
	client := &apiClient{host: host, token: "tok", http: server.Client()}

	err := client.StopJob(context.Background(), "job-123")
	if err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Error("stop endpoint was not called")
	}
}

func TestResolveByJobIDRejectsNonCrabboxJob(t *testing.T) {
	debugKeyHit := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/v1alpha/jobs/job-123" {
			json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]string{
					"name": "deploy",
				},
				"status": map[string]any{
					"state": "RUNNING",
					"agent": map[string]any{
						"ip": "1.2.3.4",
						"ports": []map[string]any{
							{"name": "ssh", "number": 40010},
						},
					},
				},
			})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/debug_ssh_key") {
			debugKeyHit = true
		}
		w.WriteHeader(404)
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "https://")
	backend := &semaphoreBackend{
		spec:   Provider{}.Spec(),
		cfg:    testConfig(host),
		rt:     testRuntime(server.Client()),
		client: &apiClient{host: host, token: "tok", http: server.Client()},
	}

	_, err := backend.resolveByJobID(context.Background(), "job-123", false, false)
	if err == nil || !strings.Contains(err.Error(), "not Crabbox-managed") {
		t.Fatalf("resolve error = %v, want Crabbox-managed rejection", err)
	}
	if debugKeyHit {
		t.Fatal("debug SSH key endpoint should not be called for non-Crabbox jobs")
	}
}

func TestResolveByJobIDRejectsIncompleteSSHTarget(t *testing.T) {
	tests := []struct {
		name  string
		ip    string
		ports []map[string]any
	}{
		{
			name:  "empty ip",
			ip:    "",
			ports: []map[string]any{{"name": "ssh", "number": 40010}},
		},
		{
			name:  "missing ssh port",
			ip:    "1.2.3.4",
			ports: []map[string]any{{"name": "http", "number": 80}},
		},
		{
			name:  "zero ssh port",
			ip:    "1.2.3.4",
			ports: []map[string]any{{"name": "ssh", "number": 0}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			debugKeyHit := false
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == "/api/v1alpha/jobs/job-123" {
					json.NewEncoder(w).Encode(map[string]any{
						"metadata": map[string]string{
							"name": "crabbox testbox blue-lobster",
						},
						"status": map[string]any{
							"state": "RUNNING",
							"agent": map[string]any{
								"ip":    tt.ip,
								"ports": tt.ports,
							},
						},
					})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/debug_ssh_key") {
					debugKeyHit = true
				}
				w.WriteHeader(404)
			}))
			defer server.Close()

			host := strings.TrimPrefix(server.URL, "https://")
			backend := &semaphoreBackend{
				spec:   Provider{}.Spec(),
				cfg:    testConfig(host),
				rt:     testRuntime(server.Client()),
				client: &apiClient{host: host, token: "tok", http: server.Client()},
			}

			_, err := backend.resolveByJobID(context.Background(), "job-123", false, false)
			if err == nil || !strings.Contains(err.Error(), "SSH endpoint is not ready") {
				t.Fatalf("resolve error = %v, want SSH endpoint readiness error", err)
			}
			if debugKeyHit {
				t.Fatal("debug SSH key endpoint should not be called before SSH endpoint is ready")
			}
		})
	}
}

func TestResolveByJobIDReleaseOnlyAllowsIncompleteSSHTarget(t *testing.T) {
	debugKeyHit := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/v1alpha/jobs/job-123" {
			json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]string{
					"name": "crabbox testbox blue-lobster",
				},
				"status": map[string]any{
					"state": "RUNNING",
					"agent": map[string]any{
						"ip":    "",
						"ports": []map[string]any{},
					},
				},
			})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/debug_ssh_key") {
			debugKeyHit = true
		}
		w.WriteHeader(404)
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "https://")
	backend := &semaphoreBackend{
		spec:   Provider{}.Spec(),
		cfg:    testConfig(host),
		rt:     testRuntime(server.Client()),
		client: &apiClient{host: host, token: "tok", http: server.Client()},
	}

	target, err := backend.resolveByJobID(context.Background(), "job-123", true, false)
	if err != nil {
		t.Fatalf("resolve release-only: %v", err)
	}
	if target.LeaseID != "sem_job-123" || target.Server.CloudID != "job-123" || target.SSH.Host != "" {
		t.Fatalf("target=%#v, want release-only identity without SSH target", target)
	}
	if debugKeyHit {
		t.Fatal("debug SSH key endpoint should not be called for release-only resolution")
	}
}

func TestResolveByJobIDReleaseOnlySkipsReadySSHTarget(t *testing.T) {
	debugKeyHit := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/v1alpha/jobs/job-123" {
			json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]string{
					"name": "crabbox testbox blue-lobster",
				},
				"status": map[string]any{
					"state": "RUNNING",
					"agent": map[string]any{
						"ip": "1.2.3.4",
						"ports": []map[string]any{
							{"name": "ssh", "number": 40010},
						},
					},
				},
			})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/debug_ssh_key") {
			debugKeyHit = true
		}
		w.WriteHeader(404)
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "https://")
	backend := &semaphoreBackend{
		spec:   Provider{}.Spec(),
		cfg:    testConfig(host),
		rt:     testRuntime(server.Client()),
		client: &apiClient{host: host, token: "tok", http: server.Client()},
	}

	target, err := backend.resolveByJobID(context.Background(), "job-123", true, false)
	if err != nil {
		t.Fatalf("resolve release-only: %v", err)
	}
	if target.LeaseID != "sem_job-123" || target.Server.PublicNet.IPv4.IP != "1.2.3.4" || target.SSH.Host != "" {
		t.Fatalf("target=%#v, want release-only server identity without SSH target", target)
	}
	if debugKeyHit {
		t.Fatal("debug SSH key endpoint should not be called for release-only resolution")
	}
}

func TestResolveStatusOnlyAllowsIncompleteSSHTarget(t *testing.T) {
	debugKeyHit := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/v1alpha/jobs/job-123" {
			json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]string{
					"name": "crabbox testbox blue-lobster",
				},
				"status": map[string]any{
					"state": "RUNNING",
					"agent": map[string]any{
						"ip":    "",
						"ports": []map[string]any{},
					},
				},
			})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/debug_ssh_key") {
			debugKeyHit = true
		}
		w.WriteHeader(404)
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "https://")
	backend := &semaphoreBackend{
		spec:   Provider{}.Spec(),
		cfg:    testConfig(host),
		rt:     testRuntime(server.Client()),
		client: &apiClient{host: host, token: "tok", http: server.Client()},
	}

	target, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "sem_job-123", StatusOnly: true})
	if err != nil {
		t.Fatalf("resolve status-only: %v", err)
	}
	if target.LeaseID != "sem_job-123" || target.SSH.Host != "" {
		t.Fatalf("target=%#v, want status-only identity without SSH target", target)
	}
	if debugKeyHit {
		t.Fatal("debug SSH key endpoint should not be called for status-only resolution")
	}
}

func TestResolveStatusKeepsReadySSHTarget(t *testing.T) {
	for _, readyProbe := range []bool{false, true} {
		t.Run(fmt.Sprintf("ready_probe_%t", readyProbe), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			debugKeyHit := false
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == "/api/v1alpha/jobs/job-123" {
					json.NewEncoder(w).Encode(map[string]any{
						"metadata": map[string]string{
							"name": "crabbox testbox blue-lobster",
						},
						"status": map[string]any{
							"state": "RUNNING",
							"agent": map[string]any{
								"ip": "1.2.3.4",
								"ports": []map[string]any{
									{"name": "ssh", "number": 40010},
								},
							},
						},
					})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/debug_ssh_key") {
					debugKeyHit = true
					json.NewEncoder(w).Encode(map[string]string{"key": "test-private-key"})
					return
				}
				w.WriteHeader(404)
			}))
			defer server.Close()

			host := strings.TrimPrefix(server.URL, "https://")
			backend := &semaphoreBackend{
				spec:   Provider{}.Spec(),
				cfg:    testConfig(host),
				rt:     testRuntime(server.Client()),
				client: &apiClient{host: host, token: "tok", http: server.Client()},
			}

			target, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "sem_job-123", StatusOnly: true, ReadyProbe: readyProbe})
			if err != nil {
				t.Fatalf("resolve status: %v", err)
			}
			if target.SSH.Host != "1.2.3.4" || target.SSH.Port != "40010" {
				t.Fatalf("ssh target=%#v, want ready SSH target", target.SSH)
			}
			if !debugKeyHit {
				t.Fatal("debug SSH key endpoint should be called for ready status resolution")
			}
		})
	}
}

func TestResolveIgnoresOtherProviderClaims(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	if err := core.ClaimLeaseForRepoProvider("cbx_123", "blue-lobster", "aws", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	backend := &semaphoreBackend{cfg: testConfig("semaphore.example.test")}
	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "blue-lobster"})
	if err == nil || !strings.Contains(err.Error(), "semaphore lease not found") {
		t.Fatalf("resolve error = %v, want semaphore not found", err)
	}
}

func TestListFiltersNonCrabboxJobs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	if err := core.ClaimLeaseForRepoProvider("sem_job-crabbox", "blue-lobster", providerName, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/v1alpha/jobs" && r.URL.Query().Get("states") == "RUNNING" {
			json.NewEncoder(w).Encode(map[string]any{
				"jobs": []map[string]any{
					{
						"metadata": map[string]string{"id": "job-crabbox", "name": "crabbox testbox"},
						"status":   map[string]string{"state": "RUNNING"},
					},
					{
						"metadata": map[string]string{"id": "job-deploy", "name": "deploy"},
						"status":   map[string]string{"state": "RUNNING"},
					},
				},
			})
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "https://")
	backend := &semaphoreBackend{
		spec:   Provider{}.Spec(),
		cfg:    testConfig(host),
		rt:     testRuntime(server.Client()),
		client: &apiClient{host: host, token: "tok", http: server.Client()},
	}

	servers, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 {
		t.Fatalf("servers = %v, want exactly one Crabbox job", servers)
	}
	if servers[0].CloudID != "job-crabbox" {
		t.Errorf("cloud id = %q, want job-crabbox", servers[0].CloudID)
	}
	if servers[0].Name != "sem-testbox-blue-lobster" {
		t.Errorf("name = %q, want sem-testbox-blue-lobster", servers[0].Name)
	}
	if servers[0].Labels["slug"] != "blue-lobster" {
		t.Errorf("slug = %q, want blue-lobster", servers[0].Labels["slug"])
	}
}

func TestReleaseRemovesClaimAndStoredKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	leaseID := "sem_job-release"
	if err := core.ClaimLeaseForRepoProvider(leaseID, "blue-lobster", providerName, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	keyPath, err := storeSSHKey(leaseID, "test-key")
	if err != nil {
		t.Fatal(err)
	}

	stopped := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/v1alpha/jobs/job-release/stop" {
			stopped = true
			w.Write([]byte("{}"))
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "https://")
	backend := &semaphoreBackend{
		spec:   Provider{}.Spec(),
		cfg:    testConfig(host),
		rt:     testRuntime(server.Client()),
		client: &apiClient{host: host, token: "tok", http: server.Client()},
	}

	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{LeaseID: leaseID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("stop endpoint was not called")
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("stored key still exists or stat failed with unexpected error: %v", err)
	}
	if _, found, err := core.ResolveLeaseClaim("blue-lobster"); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("lease claim still resolves after release")
	}
}
