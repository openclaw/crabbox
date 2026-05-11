package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
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
			_ = json.NewEncoder(w).Encode(CoordinatorWhoami{Auth: "token", Owner: "peter@example.test", Org: "openclaw"})
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
