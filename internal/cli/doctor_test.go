package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCoordinatorProviderReadinessSupported(t *testing.T) {
	tests := []struct {
		provider string
		want     bool
	}{
		{provider: "aws", want: true},
		{provider: "azure", want: true},
		{provider: "gcp", want: true},
		{provider: "hetzner", want: true},
		{provider: "proxmox", want: false},
		{provider: "daytona", want: false},
		{provider: "islo", want: false},
		{provider: "e2b", want: false},
		{provider: "blacksmith-testbox", want: false},
		{provider: "namespace-devbox", want: false},
		{provider: "semaphore", want: false},
		{provider: "sprites", want: false},
		{provider: "cloudflare", want: false},
		{provider: "ssh", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := coordinatorProviderReadinessSupported(tt.provider); got != tt.want {
				t.Fatalf("coordinatorProviderReadinessSupported(%q)=%t want %t", tt.provider, got, tt.want)
			}
		})
	}
}

func TestDoctorLocalToolsAreProviderAware(t *testing.T) {
	tests := []struct {
		name string
		spec ProviderSpec
		want []string
	}{
		{
			name: "delegated no local ssh sync",
			spec: ProviderSpec{Kind: ProviderKindDelegatedRun},
			want: []string{"git"},
		},
		{
			name: "ssh lease sync",
			spec: ProviderSpec{Kind: ProviderKindSSHLease, Features: FeatureSet{FeatureSSH, FeatureCrabboxSync}},
			want: []string{"git", "ssh", "ssh-keygen", "rsync"},
		},
		{
			name: "archive sync requires tar but not rsync",
			spec: ProviderSpec{Kind: ProviderKindDelegatedRun, Features: FeatureSet{FeatureArchiveSync}},
			want: []string{"git", "tar"},
		},
		{
			name: "provider-owned local archive sync requires tar",
			spec: ProviderSpec{Name: "e2b", Kind: ProviderKindDelegatedRun},
			want: []string{"git", "tar"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := doctorLocalTools(tt.spec); !slices.Equal(got, tt.want) {
				t.Fatalf("tools=%v want %v", got, tt.want)
			}
		})
	}
}

func TestDoctorRunsDirectProviderCheckForCoordinatorNeverProvider(t *testing.T) {
	for _, tool := range []string{"git", "ssh", "ssh-keygen", "rsync", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("missing local doctor tool %s: %v", tool, err)
		}
	}
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")

	coordinatorCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		coordinatorCalled = true
		http.Error(w, "coordinator should not be checked for direct provider", http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "token")

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), []string{"--provider", "cloudflare"})
	if err != nil {
		t.Fatalf("doctor error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "ok      provider provider=cloudflare timeout=10s direct_check=ready") {
		t.Fatalf("doctor did not run direct provider check: %q", stdout.String())
	}
	if coordinatorCalled {
		t.Fatalf("doctor checked coordinator for direct provider: %q", stdout.String())
	}
}

func TestDoctorJSONCoordinatorOutput(t *testing.T) {
	for _, tool := range []string{"git", "ssh", "ssh-keygen", "rsync"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("missing local doctor tool %s: %v", tool, err)
		}
	}
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(CoordinatorWhoami{Auth: "token", Owner: "alice@example.test", Org: "example-org"})
		case "/v1/providers/aws/readiness":
			_ = json.NewEncoder(w).Encode(CoordinatorProviderReadiness{Provider: "aws", Configured: true})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "token")

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), []string{"--provider", "aws", "--json"})
	if err != nil {
		t.Fatalf("doctor error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	var view doctorJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
		t.Fatalf("doctor JSON invalid: %v\n%s", err, stdout.String())
	}
	if !view.OK || view.Provider != "aws" {
		t.Fatalf("view=%#v", view)
	}
	found := false
	for _, check := range view.Checks {
		if check.Check == "provider" && check.Details["provider"] == "aws" && check.Details["coordinator_secrets"] == "ready" {
			found = true
		}
	}
	if !found {
		t.Fatalf("provider readiness missing from JSON: %#v", view.Checks)
	}
}

func TestDoctorSkipsProviderReadinessForCoordinatorUnsupportedProvider(t *testing.T) {
	for _, tool := range []string{"git", "ssh", "ssh-keygen", "rsync", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("missing local doctor tool %s: %v", tool, err)
		}
	}
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")

	readinessCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(CoordinatorWhoami{Auth: "token", Owner: "alice@example.test", Org: "example-org"})
		default:
			if strings.Contains(r.URL.Path, "/readiness") {
				readinessCalled = true
			}
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "token")

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), []string{"--provider", "daytona"})
	if err != nil {
		t.Fatalf("doctor error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if readinessCalled {
		t.Fatal("doctor called provider readiness for coordinator-unsupported provider")
	}
	if strings.Contains(stdout.String(), "failed  provider") {
		t.Fatalf("unexpected provider failure: %q", stdout.String())
	}
}
