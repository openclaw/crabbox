package neevcloud

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestSandboxLabels(t *testing.T) {
	all := map[string]string{
		"crabbox": "true", "provider": providerName, "lease": "cbx_0123456789abcdef", "slug": "fast-coral",
		"created_at": "2026-10-09T10:00:00Z", "repo": "example-org/my-app", "workdir": "/workspace/crabbox",
		"keep": "false", "server_type": "sb-ubuntu-26-04-dev", "unknown": "x",
	}
	for i := 0; i < 30; i++ {
		all["extra"+strconv.Itoa(i)] = "v"
	}
	got := sandboxLabels(all)
	want := map[string]string{
		"crabbox": "true", "provider": providerName, "lease": "cbx_0123456789abcdef", "slug": "fast-coral",
		"keep": "false", "server_type": "sb-ubuntu-26-04-dev",
	}
	if len(got) != len(want) {
		t.Fatalf("labels=%v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("labels[%s]=%q, want %q (all=%v)", k, got[k], v, got)
		}
	}
	if len(sandboxLabels(map[string]string{})) != 0 {
		t.Fatal("empty input must yield no labels")
	}
}

func TestSandboxName(t *testing.T) {
	dns := regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)
	tests := []struct {
		name  string
		slug  string
		lease string
		want  string
	}{
		{name: "success: slug and lease", slug: "fast-coral", lease: "cbx_0123456789abcdef", want: "crabbox-fast-coral-0123456789ab"},
		{name: "success: no slug", slug: "", lease: "cbx_0123456789ab", want: "crabbox-0123456789ab"},
		{name: "success: long slug is trimmed", slug: strings.Repeat("a", 80), lease: "cbx_0123456789ab"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sandboxName(tt.slug, tt.lease)
			if tt.want != "" && got != tt.want {
				t.Fatalf("name=%q, want %q", got, tt.want)
			}
			if len(got) > maxNameLength || !dns.MatchString(got) {
				t.Fatalf("name=%q is not a valid sandbox name", got)
			}
			if got != sandboxName(tt.slug, tt.lease) {
				t.Fatal("name must be stable for a retried create")
			}
		})
	}
}

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*core.Config)
		wantErr string
	}{
		{name: "success: defaults", mutate: func(*core.Config) {}},
		{name: "success: loopback http for development", mutate: func(c *core.Config) { c.Neevcloud.BaseURL = "http://127.0.0.1:8080" }},
		{name: "failure: plain http", mutate: func(c *core.Config) { c.Neevcloud.BaseURL = "http://api.example.com" }, wantErr: "HTTPS"},
		{name: "failure: userinfo in base URL", mutate: func(c *core.Config) { c.Neevcloud.BaseURL = "https://u:p@api.example.com" }, wantErr: "userinfo"},
		{name: "failure: workdir outside workspace", mutate: func(c *core.Config) { c.Neevcloud.Workdir = "/tmp/x" }, wantErr: "subdirectory of /workspace"},
		{name: "failure: workdir is workspace root", mutate: func(c *core.Config) { c.Neevcloud.Workdir = "/workspace" }, wantErr: "subdirectory of /workspace"},
		{name: "failure: workdir escapes", mutate: func(c *core.Config) { c.Neevcloud.Workdir = "/workspace/../etc" }, wantErr: "subdirectory of /workspace"},
		{name: "failure: negative timeout", mutate: func(c *core.Config) { c.Neevcloud.TimeoutSecs = -1 }, wantErr: "non-negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg core.Config
			tt.mutate(&cfg)
			err := validateConfig(cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err=%v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestClaimScopeSeparatesEndpointsAndProjects(t *testing.T) {
	first := testConfig("https://api-a.example.test")
	second := testConfig("https://api-b.example.test")
	other := first
	other.Neevcloud.ProjectID = "prj-2"
	if claimScope(first) == claimScope(second) || claimScope(first) == claimScope(other) {
		t.Fatalf("scopes collide: %q %q %q", claimScope(first), claimScope(second), claimScope(other))
	}
}

func TestNewClientRequiresKeyAndScope(t *testing.T) {
	t.Setenv("CRABBOX_NEEVCLOUD_API_KEY", "")
	t.Setenv("NEEV_API_KEY", "")
	if _, err := newClient(testConfig(""), core.Runtime{}); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("missing key err=%v", err)
	}
	t.Setenv("NEEV_API_KEY", testKey)
	cfg := testConfig("")
	cfg.Neevcloud.ProjectID = " "
	if _, err := newClient(cfg, core.Runtime{}); err == nil || !strings.Contains(err.Error(), "projectId") {
		t.Fatalf("missing project err=%v", err)
	}
}

func TestDoctorReportsEachCheck(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		wantCheck string
		wantState string
	}{
		{name: "success: ready", status: http.StatusOK, wantCheck: "project", wantState: "ok"},
		{name: "failure: rejected key", status: http.StatusUnauthorized, wantCheck: "auth", wantState: "blocked"},
		{name: "failure: unknown project", status: http.StatusNotFound, wantCheck: "project", wantState: "blocked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.status != http.StatusOK {
					writeJSON(w, tt.status, map[string]string{"error": "no"})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"total": 0, "items": []any{}})
			}))
			defer server.Close()
			t.Setenv("CRABBOX_NEEVCLOUD_API_KEY", testKey)
			b := &backend{cfg: testConfig(server.URL), rt: core.Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard}}
			result, err := b.Doctor(context.Background(), core.DoctorRequest{})
			if (tt.wantState == "ok") != (err == nil) {
				t.Fatalf("err=%v", err)
			}
			last := result.Checks[len(result.Checks)-1]
			if last.Check != tt.wantCheck || last.Status != tt.wantState {
				t.Fatalf("last check=%+v, want %s=%s (all=%+v)", last, tt.wantCheck, tt.wantState, result.Checks)
			}
		})
	}
}

func TestDoctorReportsUnreachableEndpoint(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()
	t.Setenv("CRABBOX_NEEVCLOUD_API_KEY", testKey)
	b := &backend{cfg: testConfig(url), rt: core.Runtime{}}
	result, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err == nil || result.Status != "blocked" || result.Checks[len(result.Checks)-1].Check != "endpoint" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
