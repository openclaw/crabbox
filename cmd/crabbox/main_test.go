package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
