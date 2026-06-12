package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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
		{provider: "morph", want: false},
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

func TestDoctorRejectsUnsupportedProviderTarget(t *testing.T) {
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

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), []string{"--provider", "xcp-ng", "--target", "macos"})
	if err == nil || !strings.Contains(err.Error(), "provider=xcp-ng managed provisioning supports target=linux only") {
		t.Fatalf("doctor error=%v stdout=%q stderr=%q, want xcp-ng target rejection", err, stdout.String(), stderr.String())
	}
}

func TestDoctorAllowsExistingLeaseDespiteUnsupportedProvisioningTarget(t *testing.T) {
	for _, tool := range []string{"git", "ssh", "ssh-keygen", "rsync"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("missing local doctor tool %s: %v", tool, err)
		}
	}
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	configPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte(`provider: xcp-ng
target: macos
xcpNg:
  apiUrl: https://xcp.example.test
  username: root
  password: secret
  template: ubuntu
  sr: local
`), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "ssh"), []byte(`#!/bin/sh
printf 'git=git version 2.40.0\n'
printf 'rsync=rsync version 3.2.7\n'
printf 'curl=curl 8.0.0\n'
printf 'jq=jq-1.7\n'
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), []string{"--provider", "xcp-ng", "--id", "cbx_existing"})
	if err != nil {
		t.Fatalf("doctor --id error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "provider=xcp-ng managed provisioning supports target=linux only") {
		t.Fatalf("doctor --id rejected stale provisioning target: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "ok      remote   cbx_existing") {
		t.Fatalf("doctor --id did not continue to remote checks: %q", stdout.String())
	}
}

func TestDoctorDoesNotPrepareExistingLease(t *testing.T) {
	for _, tool := range []string{"git", "ssh", "ssh-keygen", "rsync"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("missing local doctor tool %s: %v", tool, err)
		}
	}
	clearConfigEnv(t)
	runPrepareTestResolveRequests = nil

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), []string{
		"--provider", "run-prepare-test",
		"--id", "cbx_existing",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 9 || !strings.Contains(exitErr.Message, "resolve captured") {
		t.Fatalf("doctor error=%v stdout=%q stderr=%q, want resolve-captured exit", err, stdout.String(), stderr.String())
	}
	if len(runPrepareTestResolveRequests) != 1 {
		t.Fatalf("resolve requests=%#v, want one", runPrepareTestResolveRequests)
	}
	if got := runPrepareTestResolveRequests[0]; got.ID != "cbx_existing" || got.Prepare {
		t.Fatalf("resolve request=%#v, want existing id without Prepare", got)
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

func TestDoctorFromRunAppliesRecordedContext(t *testing.T) {
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
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs/run_123" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"run": CoordinatorRun{
			ID:         "run_123",
			Provider:   "proxmox",
			TargetOS:   targetLinux,
			Class:      "standard",
			ServerType: "vm-large",
			Command:    []string{"go", "test", "./..."},
			State:      "failed",
			Phase:      "test",
			StartedAt:  "2026-05-01T00:00:00Z",
		}})
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), []string{"--from-run", "run_123"})
	if err != nil {
		t.Fatalf("doctor error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	text := stdout.String()
	for _, want := range []string{
		"warning run      run=run_123 provider=proxmox target=linux class=standard type=vm-large lease=- phase=test missing=leaseID",
		"skip    remote   from_run=run_123 lease=missing remote_checks=skipped",
		"skip    provider provider=proxmox direct_doctor=unsupported",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("doctor --from-run output missing %q:\n%s", want, text)
		}
	}
}

func TestDoctorDirectProviderCheckIncludesTimeoutWhenMessageHasProvider(t *testing.T) {
	for _, tool := range doctorLocalTools(testCloudflareProvider{}.Spec()) {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("missing local doctor tool %s: %v", tool, err)
		}
	}
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	testCloudflareDoctorResult = &DoctorResult{
		Provider: "cloudflare",
		Checks: []DoctorCheck{{
			Status:  "ok",
			Check:   "provider",
			Message: "provider=cloudflare direct_check=ready",
			Details: map[string]string{"provider": "cloudflare"},
		}},
	}
	defer func() { testCloudflareDoctorResult = nil }()

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), []string{"--provider", "cloudflare"})
	if err != nil {
		t.Fatalf("doctor error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "ok      provider provider=cloudflare timeout=10s direct_check=ready") {
		t.Fatalf("doctor provider check did not include timeout: %q", stdout.String())
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

func TestDoctorJSONCoordinatorOutputIncludesCapacityWarnings(t *testing.T) {
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

	var readinessQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(CoordinatorWhoami{Auth: "token", Owner: "alice@example.test", Org: "example-org"})
		case "/v1/providers/aws/readiness":
			readinessQuery = r.URL.Query()
			_ = json.NewEncoder(w).Encode(CoordinatorProviderReadiness{
				Provider:   "aws",
				Configured: true,
				Checks: []DoctorCheck{{
					Status:  "warning",
					Check:   "capacity",
					Message: "provider=aws capacity=quota_pressure market=spot recommended_class=standard",
					Details: map[string]string{"provider": "aws", "market": "spot", "recommended_class": "standard"},
				}},
			})
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
	if readinessQuery.Get("class") != "beast" || readinessQuery.Get("serverType") != "c7a.48xlarge" {
		t.Fatalf("readiness query=%s, want default AWS class/type", readinessQuery.Encode())
	}
	var view doctorJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
		t.Fatalf("doctor JSON invalid: %v\n%s", err, stdout.String())
	}
	if !view.OK {
		t.Fatalf("capacity warning should not fail doctor: %#v", view)
	}
	found := false
	for _, check := range view.Checks {
		if check.Check == "capacity" && check.Status == "warning" && check.Details["recommended_class"] == "standard" {
			found = true
		}
	}
	if !found {
		t.Fatalf("capacity warning missing from JSON: %#v", view.Checks)
	}
}

func TestDoctorBrokerAuthFailureSkipsProviderReadiness(t *testing.T) {
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

	readinessCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "/v1/whoami":
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		case "/v1/providers/aws/readiness":
			readinessCalled = true
			http.Error(w, "provider readiness should not be checked after broker auth fails", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "expired-user-token")

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), []string{"--provider", "aws", "--json"})
	if err == nil {
		t.Fatalf("doctor succeeded unexpectedly stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if readinessCalled {
		t.Fatal("doctor called provider readiness after broker auth failed")
	}
	var view doctorJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
		t.Fatalf("doctor JSON invalid: %v\n%s", err, stdout.String())
	}
	if view.OK {
		t.Fatalf("view.OK=true, want false: %#v", view)
	}
	foundBrokerHint := false
	for _, check := range view.Checks {
		if check.Check == "broker" && check.Details["class"] == "broker_auth" && check.Details["hint"] == "renew_crabbox_broker_login" {
			foundBrokerHint = true
		}
		if check.Check == "provider" && strings.Contains(check.Message, "check_aws_credentials") {
			t.Fatalf("provider check leaked misleading AWS hint: %#v", check)
		}
	}
	if !foundBrokerHint {
		t.Fatalf("missing broker auth hint: %#v", view.Checks)
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
