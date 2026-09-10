package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDoctorChecksStatus(t *testing.T) {
	tests := []struct {
		name   string
		checks []DoctorCheck
		want   string
	}{
		{name: "nil checks", want: "ok"},
		{name: "empty checks", checks: []DoctorCheck{}, want: "ok"},
		{name: "absent status", checks: []DoctorCheck{{Check: "config"}}, want: "ok"},
		{name: "empty status", checks: []DoctorCheck{{Status: ""}}, want: "ok"},
		{name: "whitespace status", checks: []DoctorCheck{{Status: " \t\n"}}, want: "ok"},
		{name: "literal missing", checks: []DoctorCheck{{Status: "missing"}}, want: "failed"},
		{name: "ok", checks: []DoctorCheck{{Status: "ok"}}, want: "ok"},
		{name: "failed", checks: []DoctorCheck{{Status: "failed"}}, want: "failed"},
		{name: "warning", checks: []DoctorCheck{{Status: "warning"}}, want: "warning"},
		{name: "skip", checks: []DoctorCheck{{Status: "skip"}}, want: "ok"},
		{name: "unknown statuses", checks: []DoctorCheck{{Status: "unknown"}, {Status: "error"}, {Status: "blocked"}}, want: "ok"},
		{name: "failure words are not statuses", checks: []DoctorCheck{{Status: "missing tool"}, {Status: "not failed"}, {Status: "warnings"}}, want: "ok"},
		{name: "normalized ok and skip", checks: []DoctorCheck{{Status: " OK "}, {Status: "\tSkIp\n"}}, want: "ok"},
		{name: "normalized failed", checks: []DoctorCheck{{Status: "\tFaIlEd\n"}}, want: "failed"},
		{name: "normalized missing", checks: []DoctorCheck{{Status: " MiSsInG "}}, want: "failed"},
		{name: "normalized warning", checks: []DoctorCheck{{Status: "\nWaRnInG\t"}}, want: "warning"},
		{name: "warning then failure", checks: []DoctorCheck{{Status: "warning"}, {Status: "failed"}}, want: "failed"},
		{name: "failure then warning", checks: []DoctorCheck{{Status: "failed"}, {Status: "warning"}}, want: "failed"},
		{name: "warning then missing", checks: []DoctorCheck{{Status: " WARNING "}, {Status: " MiSsInG "}}, want: "failed"},
		{name: "missing then warning", checks: []DoctorCheck{{Status: " MiSsInG "}, {Status: " WARNING "}}, want: "failed"},
		{name: "nonfailures do not clear warning", checks: []DoctorCheck{{Status: "warning"}, {}, {Status: "skip"}, {Status: "unknown"}, {Status: "ok"}}, want: "warning"},
		{name: "duplicate successes", checks: []DoctorCheck{{Status: "ok"}, {Status: "ok"}}, want: "ok"},
		{name: "duplicate warnings", checks: []DoctorCheck{{Status: "warning"}, {Status: "warning"}}, want: "warning"},
		{name: "duplicate failures", checks: []DoctorCheck{{Status: "failed"}, {Status: "failed"}}, want: "failed"},
		{
			name: "raw checks and details survive warning",
			checks: []DoctorCheck{
				{Status: " WaRnInG ", Check: " network ", Message: " advisory\n", Details: map[string]string{"note": " raw value ", "mutation": "false"}},
				{Status: "", Check: "empty details", Details: map[string]string{}},
				{Status: " SkIp ", Check: "nil details"},
			},
			want: "warning",
		},
		{
			name: "raw checks and details survive failure",
			checks: []DoctorCheck{
				{Status: " MiSsInG ", Check: " config ", Message: " unavailable\n", Details: map[string]string{"note": " raw value "}},
				{Status: " WaRnInG ", Check: "later check", Details: map[string]string{"note": " retain me "}},
			},
			want: "failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := slices.Clone(tt.checks)
			for i := range before {
				before[i].Details = maps.Clone(tt.checks[i].Details)
			}
			if got := DoctorChecksStatus(tt.checks); got != tt.want {
				t.Errorf("DoctorChecksStatus() = %q, want %q", got, tt.want)
			}
			if !reflect.DeepEqual(tt.checks, before) {
				t.Errorf("checks changed: got %#v, want %#v", tt.checks, before)
			}
		})
	}
}

func TestDoctorStatusFails(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   bool
	}{
		{name: "absent status", want: false},
		{name: "empty status", status: "", want: false},
		{name: "whitespace status", status: " \t\n", want: false},
		{name: "literal missing", status: "missing", want: true},
		{name: "failed", status: "failed", want: true},
		{name: "ok", status: "ok", want: false},
		{name: "advisory warning", status: "warning", want: false},
		{name: "skip", status: "skip", want: false},
		{name: "unknown", status: "unknown", want: false},
		{name: "error is not failed", status: "error", want: false},
		{name: "local blocked is not failed", status: "blocked", want: false},
		{name: "missing phrase is not missing", status: "missing tool", want: false},
		{name: "failed phrase is not failed", status: "not failed", want: false},
		{name: "padded failed", status: " failed\t", want: true},
		{name: "mixed case failed", status: "FaIlEd", want: true},
		{name: "normalized failed", status: "\tFaIlEd\n", want: true},
		{name: "padded missing", status: " missing\n", want: true},
		{name: "mixed case missing", status: "MiSsInG", want: true},
		{name: "normalized missing", status: "\nMiSsInG\t", want: true},
		{name: "normalized warning", status: " WaRnInG ", want: false},
		{name: "normalized skip", status: " SkIp ", want: false},
		{name: "normalized ok", status: " OK ", want: false},
		{name: "normalized unknown", status: " UNKNOWN ", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := doctorStatusFails(tt.status); got != tt.want {
				t.Errorf("doctorStatusFails(%q) = %t, want %t", tt.status, got, tt.want)
			}
		})
	}
}

func TestCoordinatorProviderReadinessSupported(t *testing.T) {
	tests := []struct {
		provider string
		want     bool
	}{
		{provider: "aws", want: true},
		{provider: "azure", want: true},
		{provider: "gcp", want: true},
		{provider: "hetzner", want: true},
		{provider: "daytona", want: true},
		{provider: "proxmox", want: false},
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

func TestDoctorProviderSelectionProvenanceAndStrictness(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		setup      func(*testing.T)
		wantSource providerSelectionSource
		wantOK     bool
	}{
		{
			name:       "compiled default",
			wantSource: providerSelectionCompiledDefault,
			wantOK:     true,
		},
		{
			name:       "provider flag",
			args:       []string{"--provider", "hetzner"},
			wantSource: providerSelectionFlag,
		},
		{
			name: "environment",
			setup: func(t *testing.T) {
				t.Setenv("CRABBOX_PROVIDER", "hetzner")
			},
			wantSource: providerSelectionEnvironment,
		},
		{
			name: "repository config",
			setup: func(t *testing.T) {
				if err := os.WriteFile(".crabbox.yaml", []byte("provider: hetzner\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantSource: providerSelectionRepoConfig,
		},
		{
			name: "user config with selected profile",
			setup: func(t *testing.T) {
				path := userConfigPath()
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				body := "provider: hetzner\nprofile: qa\nprofiles:\n  qa:\n    doctor:\n      enabled: false\n"
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantSource: providerSelectionUserConfig,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateDoctorProviderSelectionTest(t)
			if tt.setup != nil {
				tt.setup(t)
			}
			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), tt.args)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("doctor error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
				}
				if !strings.Contains(stdout.String(), "provider-selection no provider selected source=compiled_default selected=false") ||
					!strings.Contains(stdout.String(), "warning provider no provider selected source=compiled_default selected=false readiness=skipped") {
					t.Fatalf("doctor did not skip compiled-default readiness:\n%s", stdout.String())
				}
			} else {
				var exitErr ExitError
				if !AsExitError(err, &exitErr) || exitErr.Code != 1 {
					t.Fatalf("doctor error=%v, want exit 1; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
				}
				if !strings.Contains(stdout.String(), "failed  provider provider=hetzner") || !strings.Contains(stdout.String(), "HCLOUD_TOKEN or HETZNER_TOKEN is required") {
					t.Fatalf("doctor did not preserve strict provider readiness:\n%s", stdout.String())
				}
			}
			wantSelection := "provider-selection provider=hetzner source=" + string(tt.wantSource) + " selected=true"
			if tt.wantSource == providerSelectionCompiledDefault {
				wantSelection = "provider-selection no provider selected source=compiled_default selected=false"
			}
			if !strings.Contains(stdout.String(), wantSelection) {
				t.Fatalf("doctor selection provenance missing %q:\n%s", wantSelection, stdout.String())
			}
		})
	}
}

func TestDoctorCompiledDefaultSkipsCoordinatorProviderReadiness(t *testing.T) {
	isolateDoctorProviderSelectionTest(t)
	readinessCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(CoordinatorWhoami{Auth: "token", Owner: "alice@example.test", Org: "example-org"})
		case "/v1/providers/hetzner/readiness":
			readinessCalled = true
			http.Error(w, "compiled-default readiness should be skipped", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "token")

	var stdout, stderr bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), nil); err != nil {
		t.Fatalf("doctor error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if readinessCalled {
		t.Fatal("doctor called coordinator readiness for the compiled default")
	}
	for _, want := range []string{"ok      coord", "ok      broker", "warning provider no provider selected source=compiled_default selected=false readiness=skipped"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "default_type") || strings.Contains(stdout.String(), "ccx63") {
		t.Fatalf("provider-neutral doctor exposed a provider default type:\n%s", stdout.String())
	}
}

func TestDoctorCompiledDefaultJSONReportsSkippedReadiness(t *testing.T) {
	isolateDoctorProviderSelectionTest(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(CoordinatorWhoami{Auth: "token", Owner: "alice@example.test", Org: "example-org"})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "token")
	var stdout, stderr bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), []string{"--json"}); err != nil {
		t.Fatalf("doctor error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	var view doctorJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
		t.Fatalf("doctor JSON invalid: %v\n%s", err, stdout.String())
	}
	if !view.OK || view.Provider != "" {
		t.Fatalf("doctor view=%#v", view)
	}
	var selection, readiness, broker bool
	for _, check := range view.Checks {
		selection = selection || (check.Check == "provider-selection" && check.Details["provider"] == "" && check.Details["source"] == "compiled_default" && check.Details["selected"] == "false")
		readiness = readiness || (check.Check == "provider" && check.Status == "warning" && check.Details["provider"] == "" && check.Details["selected"] == "false" && check.Details["readiness"] == "skipped")
		broker = broker || check.Check == "broker"
	}
	if !selection || !readiness || !broker {
		t.Fatalf("doctor JSON missing provenance or skip: %#v", view.Checks)
	}
	for _, check := range view.Checks {
		if _, ok := check.Details["default_type"]; ok || strings.Contains(check.Message, "default_type") || strings.Contains(check.Message, "ccx63") {
			t.Fatalf("provider-neutral doctor JSON exposed a provider default type: %#v", check)
		}
	}
}

func TestDoctorExplicitProviderJSONReportsSelected(t *testing.T) {
	isolateDoctorProviderSelectionTest(t)
	var stdout, stderr bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), []string{"--provider", "ssh", "--json"}); err != nil {
		t.Fatalf("doctor error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	var view doctorJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
		t.Fatalf("doctor JSON invalid: %v\n%s", err, stdout.String())
	}
	if !view.OK || view.Provider != "ssh" {
		t.Fatalf("doctor view=%#v", view)
	}
	for _, check := range view.Checks {
		if check.Check == "provider-selection" && check.Details["provider"] == "ssh" && check.Details["source"] == "flag" && check.Details["selected"] == "true" {
			return
		}
	}
	t.Fatalf("doctor JSON missing selected provider provenance: %#v", view.Checks)
}

func TestDoctorConfiguredProviderKeepsCoordinatorReadinessStrict(t *testing.T) {
	isolateDoctorProviderSelectionTest(t)
	t.Setenv("CRABBOX_PROVIDER", "aws")
	readinessCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(CoordinatorWhoami{Auth: "token", Owner: "alice@example.test", Org: "example-org"})
		case "/v1/providers/aws/readiness":
			readinessCalled = true
			_ = json.NewEncoder(w).Encode(CoordinatorProviderReadiness{Provider: "aws", Configured: false, Missing: []string{"AWS_ACCESS_KEY_ID"}})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "token")

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), nil)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("doctor error=%v, want exit 1; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if !readinessCalled {
		t.Fatal("doctor skipped coordinator readiness for a configured provider")
	}
	for _, want := range []string{"provider-selection provider=aws source=environment", "failed  provider provider=aws missing=AWS_ACCESS_KEY_ID"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestDoctorCompiledDefaultStillFailsProviderNeutralReadinessChecks(t *testing.T) {
	isolateDoctorProviderSelectionTest(t)
	t.Setenv("PATH", doctorTestToolPath(t, nil))

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), nil)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("doctor error=%v, want exit 1; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "missing git") || !strings.Contains(stdout.String(), "readiness=skipped") || strings.Contains(stdout.String(), "missing ssh") {
		t.Fatalf("doctor did not preserve non-provider failures:\n%s", stdout.String())
	}
}

func isolateDoctorProviderSelectionTest(t *testing.T) {
	t.Helper()
	clearConfigEnv(t)
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("CRABBOX_PROFILE", "")
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("HCLOUD_TOKEN", "")
	t.Setenv("HETZNER_TOKEN", "")
	t.Chdir(t.TempDir())
	t.Setenv("PATH", doctorTestToolPath(t, []string{"git", "ssh", "ssh-keygen", "rsync"}))
}

func doctorTestToolPath(t *testing.T, tools []string) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range tools {
		path := filepath.Join(dir, tool)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return dir
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

func TestDoctorIDUsesSharedLeaseIdentifierRouting(t *testing.T) {
	t.Run("ordinary claim", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		setupClaimRoutingCommandTest(t, claimRoutingUnusableProvider)
		t.Setenv("PATH", doctorTestToolPath(t, []string{"git", "ssh", "ssh-keygen", "rsync"}))
		mustWriteClaimRoutingTestClaim(t, "cbx_doctor_claim", "doctor-claim", claimRoutingUsableProvider)

		var stdout bytes.Buffer
		err := (App{Stdout: &stdout, Stderr: io.Discard}).doctor(context.Background(), []string{"--id", "doctor-claim"})
		if err != nil {
			t.Fatalf("doctor error=%v\n%s", err, stdout.String())
		}
		if !strings.Contains(stdout.String(), "provider-selection provider="+claimRoutingUsableProvider+" source=lease_context") {
			t.Fatalf("doctor did not route ordinary claim:\n%s", stdout.String())
		}
	})

	t.Run("static claim", func(t *testing.T) {
		isolateDoctorProviderSelectionTest(t)
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		claimed := baseConfig()
		claimed.Provider = staticProvider
		claimed.Static.Host = "doctor-static.example.test"
		claimed.Static.User = "runner"
		if err := claimLeaseForRepoConfig("static_doctor", "doctor-static", claimed, "/repo", time.Minute, false); err != nil {
			t.Fatal(err)
		}

		var stdout bytes.Buffer
		err := (App{Stdout: &stdout, Stderr: io.Discard}).doctor(context.Background(), []string{"--id", "doctor-static"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout.String(), "provider-selection provider=ssh source=lease_context") {
			t.Fatalf("doctor did not route Static claim:\n%s", stdout.String())
		}
	})

	t.Run("external claim", func(t *testing.T) {
		clearConfigEnv(t)
		root := setExternalRoutingTestHome(t)
		t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
		t.Setenv("PATH", doctorTestToolPath(t, []string{"git", "ssh", "ssh-keygen", "rsync"}))
		const leaseID = "cbx_doctor_external"
		if _, err := PersistExternalRouting(leaseID, ExternalConfig{Command: "doctor-external", WorkRoot: "/work/doctor"}); err != nil {
			t.Fatal(err)
		}
		if err := claimLeaseForRepoProviderScope(leaseID, "doctor-external", "external", "doctor-scope", root, time.Minute, false); err != nil {
			t.Fatal(err)
		}
		testExternalResolveHook = func(req ResolveRequest) (LeaseTarget, error) {
			return LeaseTarget{LeaseID: leaseID, Server: Server{Provider: "external", CloudID: leaseID}, SSH: SSHTarget{Host: "doctor-external.example.test", User: "runner", Port: "22", TargetOS: targetLinux}}, nil
		}
		t.Cleanup(func() { testExternalResolveHook = nil })

		var stdout bytes.Buffer
		err := (App{Stdout: &stdout, Stderr: io.Discard}).doctor(context.Background(), []string{"--id", "doctor-external"})
		if err != nil {
			t.Fatalf("doctor error=%v\n%s", err, stdout.String())
		}
		if !strings.Contains(stdout.String(), "provider-selection provider=external source=lease_context") {
			t.Fatalf("doctor did not route External claim:\n%s", stdout.String())
		}
	})
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
		"provider-selection provider=proxmox source=recorded_run",
		"skip    remote   from_run=run_123 lease=missing remote_checks=skipped",
		"skip    provider provider=proxmox direct_doctor=unsupported",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("doctor --from-run output missing %q:\n%s", want, text)
		}
	}

	stdout.Reset()
	stderr.Reset()
	err = (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), []string{"--from-run", "run_123", "--provider", "proxmox"})
	if err != nil {
		t.Fatalf("doctor --from-run --provider error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "provider-selection provider=proxmox source=flag") {
		t.Fatalf("explicit provider did not retain flag provenance:\n%s", stdout.String())
	}
}

func TestDoctorFromRunProviderSurvivesUnrelatedIdentifierClaim(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("PATH", doctorTestToolPath(t, []string{"git", "ssh", "ssh-keygen", "rsync"}))
	if err := claimLeaseForRepoProvider("cbx_recorded_doctor", "recorded-doctor", "external", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs/run_recorded_claim" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"run": CoordinatorRun{
			ID: "run_recorded_claim", Provider: claimRoutingUsableProvider, TargetOS: targetLinux,
			Class: "standard", ServerType: "test", LeaseID: "recorded-doctor", Phase: "test",
		}})
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)

	var stdout bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: io.Discard}).doctor(context.Background(), []string{"--from-run", "run_recorded_claim"})
	if err != nil {
		t.Fatalf("doctor error=%v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "provider-selection provider="+claimRoutingUsableProvider+" source=recorded_run") {
		t.Fatalf("recorded provider was replaced by identifier claim:\n%s", stdout.String())
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
	brokerURL := strings.Replace(server.URL, "http://", "http://broker-user:broker-password@", 1)
	t.Setenv("CRABBOX_COORDINATOR", brokerURL)
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
	foundCoordinator := false
	foundBrokerDefault := false
	for _, check := range view.Checks {
		if check.Check == "coord" {
			foundCoordinator = true
			if !strings.Contains(check.Message, "http://<redacted>@") || !strings.Contains(check.Details["url"], "http://<redacted>@") {
				t.Fatalf("coordinator URL was not redacted: %#v", check)
			}
			for _, secret := range []string{"broker-user", "broker-password"} {
				if strings.Contains(check.Message, secret) || strings.Contains(check.Details["url"], secret) {
					t.Fatalf("coordinator check leaked %q: %#v", secret, check)
				}
			}
		}
		if check.Check == "provider" && check.Details["provider"] == "aws" && check.Details["coordinator_secrets"] == "ready" {
			found = true
		}
		if check.Check == "broker" && check.Details["default_type"] != "" && strings.Contains(check.Message, "default_type="+check.Details["default_type"]) {
			foundBrokerDefault = true
		}
	}
	if !foundCoordinator {
		t.Fatalf("coordinator check missing from JSON: %#v", view.Checks)
	}
	if !found {
		t.Fatalf("provider readiness missing from JSON: %#v", view.Checks)
	}
	if !foundBrokerDefault {
		t.Fatalf("selected provider default type missing from broker JSON: %#v", view.Checks)
	}
}

func TestDoctorRedactsCoordinatorAndProviderDiagnostics(t *testing.T) {
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
	const configuredSecret = "exact-doctor-secret"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(CoordinatorWhoami{Auth: "token", Owner: "alice@example.test", Org: "example-org"})
		case "/v1/providers/aws/readiness":
			_ = json.NewEncoder(w).Encode(CoordinatorProviderReadiness{
				Provider:   "aws",
				Configured: true,
				Checks: []DoctorCheck{{
					Status:  "warning",
					Check:   "diagnostic",
					Message: "quota warning exact=" + configuredSecret + " Authorization: Bearer reflected-bearer",
					Details: map[string]string{
						"url":  "https://user:pass@example.test/path?token=query-secret&region=eu",
						"json": `{"clientSecret":"json-secret","message":"detail retained"}`,
						"pem":  "-----BEGIN PRIVATE KEY-----\nprivate-material\n-----END PRIVATE KEY-----",
					},
				}},
			})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", configuredSecret)

	for _, jsonOutput := range []bool{false, true} {
		name := "text"
		args := []string{"--provider", "aws"}
		if jsonOutput {
			name = "json"
			args = append(args, "--json")
		}
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), args); err != nil {
				t.Fatalf("doctor error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
			output := stdout.String()
			for _, leaked := range []string{configuredSecret, "reflected-bearer", "user", "pass", "query-secret", "json-secret", "private-material"} {
				if strings.Contains(output, leaked) {
					t.Fatalf("doctor %s leaked %q:\n%s", name, leaked, output)
				}
			}
			preserved := []string{"quota warning", "[redacted]"}
			if jsonOutput {
				preserved = append(preserved, "detail retained", "region=eu")
			}
			for _, preserved := range preserved {
				if !strings.Contains(output, preserved) {
					t.Fatalf("doctor %s lost %q:\n%s", name, preserved, output)
				}
			}
		})
	}
}

func TestDoctorRedactsRuntimeOnlyProviderSecret(t *testing.T) {
	for _, tool := range []string{"git", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("missing local doctor tool %s: %v", tool, err)
		}
	}
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	const secret = "opaque-runtime-only-wandb-secret"
	t.Setenv("WANDB_API_KEY", secret)

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), []string{"--provider", "wandb", "--json"})
	if err != nil {
		t.Fatalf("doctor error=%v stderr=%q", err, stderr.String())
	}
	output := stdout.String()
	if strings.Contains(output, secret) || !strings.Contains(output, "[redacted]") || !strings.Contains(output, "region=eu") {
		t.Fatalf("doctor returned unsafe diagnostic: %s", output)
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

func TestDoctorBrokerAuthFailureReportsExpiredUserToken(t *testing.T) {
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
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", testCrabboxUserToken(t, time.Now().Add(-time.Hour)))

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), []string{"--provider", "aws", "--json"})
	if err == nil {
		t.Fatalf("doctor succeeded unexpectedly stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	var view doctorJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
		t.Fatalf("doctor JSON invalid: %v\n%s", err, stdout.String())
	}
	for _, check := range view.Checks {
		if check.Check != "broker" {
			continue
		}
		if check.Details["token_state"] != "expired" {
			t.Fatalf("token_state=%q details=%#v", check.Details["token_state"], check.Details)
		}
		if check.Details["token_expires"] == "" || !strings.Contains(check.Message, "token_state=expired") {
			t.Fatalf("missing expiry detail/message: %#v", check)
		}
		return
	}
	t.Fatalf("missing broker check: %#v", view.Checks)
}

func TestDoctorChecksProviderReadinessForCoordinatorSupportedDaytona(t *testing.T) {
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
		case "/v1/providers/daytona/readiness":
			readinessCalled = true
			_ = json.NewEncoder(w).Encode(CoordinatorProviderReadiness{
				Provider:   "daytona",
				Configured: true,
				Checks: []DoctorCheck{{
					Status:  "ok",
					Check:   "daytona-fallback",
					Message: "auth=ready control_plane=ready inventory=ready leases=0 mutation=false",
					Details: map[string]string{
						"client_auth":   "crabbox",
						"control_plane": "coordinator",
						"data_plane":    "ssh-rsync",
						"mutation":      "false",
					},
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
	err := (App{Stdout: &stdout, Stderr: &stderr}).doctor(context.Background(), []string{"--provider", "daytona"})
	if err != nil {
		t.Fatalf("doctor error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if !readinessCalled {
		t.Fatal("doctor did not call Daytona provider readiness")
	}
	if !strings.Contains(stdout.String(), "ok      provider provider=daytona coordinator_secrets=ready") {
		t.Fatalf("missing Daytona readiness: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "ok      daytona-fallback auth=ready control_plane=ready inventory=ready leases=0 mutation=false") {
		t.Fatalf("missing Daytona fallback readiness: %q", stdout.String())
	}
}

func testCrabboxUserToken(t *testing.T, expiresAt time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"typ":   "crabbox-user",
		"owner": "alice@example.test",
		"org":   "example-org",
		"login": "alice",
		"iat":   expiresAt.Add(-time.Hour).Unix(),
		"exp":   expiresAt.Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return "cbxu_" + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
