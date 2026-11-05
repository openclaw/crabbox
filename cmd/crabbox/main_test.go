package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw/crabbox/internal/cli"
)

func TestDoctorCloudflareUsesRealProviderReadiness(t *testing.T) {
	for _, tool := range []string{"git", "ssh", "ssh-keygen", "rsync", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("missing local doctor tool %s: %v", tool, err)
		}
	}
	clearCrabboxDoctorEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(home, "missing.yaml"))
	t.Setenv("CRABBOX_CLOUDFLARE_RUNNER_TOKEN", "runner-token")

	readinessCalled := false
	var unexpected []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/readiness" {
			if r.Method != http.MethodGet {
				http.Error(w, "wrong method", http.StatusMethodNotAllowed)
				return
			}
			if r.Header.Get("Authorization") != "Bearer runner-token" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			readinessCalled = true
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "runner": "cloudflare"})
			return
		}
		unexpected = append(unexpected, fmt.Sprintf("%s %s", r.Method, r.URL.Path))
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()

	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "coordinator-token")

	var stdout, stderr bytes.Buffer
	err := (cli.App{Stdout: &stdout, Stderr: &stderr}).Run(
		context.Background(),
		[]string{"doctor", "--provider", "cloudflare", "--cloudflare-url", server.URL},
	)
	if err != nil {
		t.Fatalf("doctor error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if !readinessCalled {
		t.Fatalf("doctor did not call real Cloudflare readiness endpoint: %q", stdout.String())
	}
	if len(unexpected) > 0 {
		t.Fatalf("doctor made unexpected requests: %v", unexpected)
	}
	text := stdout.String()
	if !strings.Contains(text, "ok      provider provider=cloudflare") ||
		!strings.Contains(text, "runner="+server.URL) ||
		!strings.Contains(text, "auth=ready") ||
		!strings.Contains(text, "api=readiness") {
		t.Fatalf("doctor output missing Cloudflare readiness result: %q", text)
	}
}

func emptyCoordinatorConfig(t *testing.T) {
	t.Helper()
	clearCrabboxDoctorEnv(t)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "CRABBOX_") {
			t.Setenv(key, "")
		}
	}
	fixture := t.TempDir()
	home := filepath.Join(fixture, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(fixture, "config.yaml")
	if err := os.WriteFile(config, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", config)
	t.Chdir(fixture)
}

func TestDoctorWithoutCoordinatorReturnsFailedJSON(t *testing.T) {
	for _, tc := range []struct {
		provider string
		target   string
	}{
		{"ssh-host", "windows"},
		{"ssh-host", "linux"},
		{"serve", "linux"},
	} {
		t.Run(tc.provider+"/"+tc.target, func(t *testing.T) {
			emptyCoordinatorConfig(t)
			var stdout, stderr bytes.Buffer
			err := (cli.App{Stdout: &stdout, Stderr: &stderr}).Run(
				t.Context(),
				[]string{"doctor", "--provider", tc.provider, "--target", tc.target, "--json"},
			)
			var exitErr cli.ExitError
			if !cli.AsExitError(err, &exitErr) || exitErr.Code != 1 {
				t.Fatalf("doctor error=%v, want exit 1; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
			var output struct {
				OK       bool   `json:"ok"`
				Provider string `json:"provider"`
				Checks   []struct {
					Status  string            `json:"status"`
					Check   string            `json:"check"`
					Details map[string]string `json:"details"`
				} `json:"checks"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatalf("doctor JSON invalid: %v\n%s", err, stdout.String())
			}
			if output.OK || output.Provider != tc.provider {
				t.Fatalf("doctor output=%#v", output)
			}
			for _, check := range output.Checks {
				if check.Check == "provider" && check.Status == "failed" &&
					strings.Contains(check.Details["error"], "coordinator") &&
					strings.Contains(check.Details["error"], "broker.url") &&
					strings.Contains(check.Details["error"], "log in") {
					return
				}
			}
			t.Fatalf("doctor JSON missing coordinator prerequisite failure: %#v", output.Checks)
		})
	}
}

func TestHostWithoutCoordinator(t *testing.T) {
	for _, action := range []string{"list", "enroll"} {
		t.Run(action, func(t *testing.T) {
			emptyCoordinatorConfig(t)
			stateDir := filepath.Join(t.TempDir(), "host-state")
			var stdout, stderr bytes.Buffer
			err := (cli.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(),
				[]string{"host", action, "--state-dir", stateDir})
			var exitErr cli.ExitError
			if !cli.AsExitError(err, &exitErr) || exitErr.Code != 2 ||
				!strings.Contains(err.Error(), "coordinator") || !strings.Contains(err.Error(), "broker.url") {
				t.Fatalf("host %s error=%v, want coordinator configuration error; stdout=%q stderr=%q", action, err, stdout.String(), stderr.String())
			}
			if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
				t.Fatalf("host %s touched agent state before checking coordinator: %v", action, err)
			}
		})
	}
}

func clearCrabboxDoctorEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"CRABBOX_PROVIDER",
		"CRABBOX_DEFAULT_CLASS",
		"CRABBOX_SERVER_TYPE",
		"CRABBOX_COORDINATOR",
		"CRABBOX_COORDINATOR_TOKEN",
		"CRABBOX_COORDINATOR_ADMIN_TOKEN",
		"CRABBOX_ADMIN_TOKEN",
		"CRABBOX_ACCESS_CLIENT_ID",
		"CRABBOX_ACCESS_CLIENT_SECRET",
		"CRABBOX_ACCESS_TOKEN",
		"CF_ACCESS_CLIENT_ID",
		"CF_ACCESS_CLIENT_SECRET",
		"CF_ACCESS_TOKEN",
		"CRABBOX_CLOUDFLARE_RUNNER_URL",
		"CRABBOX_CLOUDFLARE_RUNNER_TOKEN",
		"CRABBOX_CLOUDFLARE_WORKDIR",
	} {
		t.Setenv(key, "")
	}
}
