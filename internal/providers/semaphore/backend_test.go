package semaphore

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type testClock struct{}

func (testClock) Now() time.Time { return time.Now() }

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
		"--semaphore-token", "my-token",
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
	if cfg.Semaphore.Token != "my-token" {
		t.Errorf("token = %q", cfg.Semaphore.Token)
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
	if d := idleTimeout(cfg); d != 30*time.Minute {
		t.Errorf("default idle timeout = %v, want 30m", d)
	}
}

func TestIdleTimeoutFromConfig(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Semaphore.IdleTimeout = "15m"
	if d := idleTimeout(cfg); d != 15*time.Minute {
		t.Errorf("idle timeout = %v, want 15m", d)
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

func TestStripLeasePrefix(t *testing.T) {
	if stripLeasePrefix("sem_abc123") != "abc123" {
		t.Errorf("got %q", stripLeasePrefix("sem_abc123"))
	}
	if stripLeasePrefix("abc123") != "abc123" {
		t.Errorf("got %q", stripLeasePrefix("abc123"))
	}
}

// --- API client tests with httptest ---

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

	state, ip, port, err := client.GetJobStatus(context.Background(), "job-123")
	if err != nil {
		t.Fatal(err)
	}
	if state != "RUNNING" {
		t.Errorf("state = %q", state)
	}
	if ip != "1.2.3.4" {
		t.Errorf("ip = %q", ip)
	}
	if port != 40010 {
		t.Errorf("port = %d", port)
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
		if r.URL.Path == "/api/v1alpha/projects/my-project" {
			w.WriteHeader(400) // some hosts don't support name lookup
			return
		}
		if r.URL.Path == "/api/v1alpha/projects" {
			json.NewEncoder(w).Encode([]map[string]any{
				{"metadata": map[string]string{"name": "other", "id": "other-id"}},
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

func TestStopJob(t *testing.T) {
	stopped := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/stop") {
			stopped = true
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
