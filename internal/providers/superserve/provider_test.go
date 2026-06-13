package superserve

import (
	"context"
	"flag"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecIsDelegatedLinuxAndAliasFree(t *testing.T) {
	provider := Provider{}
	if provider.Name() != providerName {
		t.Fatalf("Name=%q want %q", provider.Name(), providerName)
	}
	if aliases := provider.Aliases(); len(aliases) != 0 {
		t.Fatalf("aliases=%v want none", aliases)
	}
	spec := provider.Spec()
	if spec.Name != providerName || spec.Family != "superserve" {
		t.Fatalf("spec identity = %#v", spec)
	}
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%q want delegated-run", spec.Kind)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%q want never", spec.Coordinator)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v want linux only", spec.Targets)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("features=%v want archive-sync and cleanup", spec.Features)
	}
}

func TestProviderForResolvesCanonicalOnly(t *testing.T) {
	got, err := core.ProviderFor("superserve")
	if err != nil {
		t.Fatalf("ProviderFor(superserve): %v", err)
	}
	if got.Name() != providerName {
		t.Fatalf("ProviderFor(superserve).Name=%q", got.Name())
	}
	for _, alias := range []string{"ss", "sup", "super-serve"} {
		if got, err := core.ProviderFor(alias); err == nil && got.Name() == providerName {
			t.Fatalf("%q alias unexpectedly resolves to superserve", alias)
		}
	}
}

func TestApplyFlagsUpdatesSuperserveConfig(t *testing.T) {
	cfg := testConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("class", "", "")
	fs.String("type", "", "")
	values := (Provider{}).RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--superserve-base-url", "https://api.example.test/",
		"--superserve-template", "superserve/custom",
		"--superserve-snapshot", "snap-123",
		"--superserve-workdir", "/workspace/custom",
		"--superserve-timeout-secs", "300",
		"--superserve-exec-timeout-secs", "120",
		"--superserve-network-allow-out", "api.example.test, pkg.example.test",
		"--superserve-network-deny-out", "169.254.169.254/32",
		"--superserve-forget-missing",
	}); err != nil {
		t.Fatal(err)
	}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatalf("ApplyFlags err=%v", err)
	}
	if cfg.Superserve.BaseURL != "https://api.example.test/" || cfg.Superserve.Template != "superserve/custom" || cfg.Superserve.Snapshot != "snap-123" || cfg.Superserve.Workdir != "/workspace/custom" || cfg.Superserve.TimeoutSecs != 300 || cfg.Superserve.ExecTimeoutSecs != 120 || !cfg.Superserve.ForgetMissing {
		t.Fatalf("cfg=%#v", cfg.Superserve)
	}
	if len(cfg.Superserve.NetworkAllowOut) != 2 || cfg.Superserve.NetworkAllowOut[1] != "pkg.example.test" || len(cfg.Superserve.NetworkDenyOut) != 1 {
		t.Fatalf("network config=%#v", cfg.Superserve)
	}
}

func TestApplyFlagsRejectsGenericSizingForSuperserve(t *testing.T) {
	for _, flagName := range []string{"class", "type"} {
		t.Run(flagName, func(t *testing.T) {
			cfg := testConfig()
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.String("class", "", "")
			fs.String("type", "", "")
			values := (Provider{}).RegisterFlags(fs, cfg)
			if err := fs.Parse([]string{"--" + flagName, "large"}); err != nil {
				t.Fatal(err)
			}
			err := (Provider{}).ApplyFlags(&cfg, fs, values)
			if err == nil || !strings.Contains(err.Error(), "--"+flagName+" is not supported for provider=superserve") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestValidateSuperserveBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "default", raw: "", want: defaultBaseURL},
		{name: "https default port", raw: "HTTPS://API.EXAMPLE.TEST:443/", want: "https://api.example.test"},
		{name: "loopback http", raw: "http://localhost:8080/", want: "http://localhost:8080"},
		{name: "IPv6 loopback", raw: "http://[::1]/", want: "http://[::1]"},
		{name: "IPv6 loopback default port", raw: "http://[::1]:80/", want: "http://[::1]"},
		{name: "userinfo", raw: "https://user:pass@api.example.test", wantErr: "must not contain userinfo"},
		{name: "query", raw: "https://api.example.test?token=secret", wantErr: "must not contain userinfo"},
		{name: "fragment", raw: "https://api.example.test/#secret", wantErr: "must not contain userinfo"},
		{name: "plain http", raw: "http://api.example.test", wantErr: "must use HTTPS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateSuperserveBaseURL(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if got != tt.want {
				t.Fatalf("got=%q want %q", got, tt.want)
			}
		})
	}
}

func TestValidateSuperserveConfigRejectsBadValues(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "relative workdir", mutate: func(cfg *Config) { cfg.Superserve.Workdir = "workspace" }, wantErr: "workdir must be absolute"},
		{name: "broad workdir", mutate: func(cfg *Config) { cfg.Superserve.Workdir = "/workspace" }, wantErr: "too broad"},
		{name: "system workdir", mutate: func(cfg *Config) { cfg.Superserve.Workdir = "/etc" }, wantErr: "too broad"},
		{name: "negative timeout", mutate: func(cfg *Config) { cfg.Superserve.TimeoutSecs = -1 }, wantErr: "timeoutSecs must be non-negative"},
		{name: "negative exec timeout", mutate: func(cfg *Config) { cfg.Superserve.ExecTimeoutSecs = -1 }, wantErr: "execTimeoutSecs must be non-negative"},
		{name: "timeout over seven days", mutate: func(cfg *Config) { cfg.Superserve.TimeoutSecs = maxSuperserveSandboxTimeoutSecs + 1 }, wantErr: "must not exceed 604800"},
		{name: "derived TTL over seven days", mutate: func(cfg *Config) { cfg.TTL = 7*24*time.Hour + time.Millisecond }, wantErr: "must not exceed 604800"},
		{name: "deny hostname", mutate: func(cfg *Config) { cfg.Superserve.NetworkDenyOut = []string{"metadata.example.test"} }, wantErr: "must be a CIDR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			tt.mutate(&cfg)
			err := validateSuperserveConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err=%v want %q", err, tt.wantErr)
			}
		})
	}
}

func TestSuperserveExecTimeoutPreservesExplicitZero(t *testing.T) {
	cfg := testConfig()
	cfg.Superserve.ExecTimeoutSecs = 0
	backend := NewSuperserveBackend((Provider{}).Spec(), cfg, Runtime{}).(*backend)
	if got := backend.execTimeoutSecs(); got != 0 {
		t.Fatalf("exec timeout=%d, want service default marker 0", got)
	}
}

func TestSuperserveSandboxTimeoutUsesConfiguredValueOrTTL(t *testing.T) {
	cfg := testConfig()
	cfg.TTL = 2*time.Minute + time.Millisecond
	backend := NewSuperserveBackend((Provider{}).Spec(), cfg, Runtime{}).(*backend)
	if got := backend.sandboxTimeoutSecs(); got != 121 {
		t.Fatalf("sandbox timeout=%d, want rounded TTL 121", got)
	}
	backend.cfg.Superserve.TimeoutSecs = 300
	if got := backend.sandboxTimeoutSecs(); got != 300 {
		t.Fatalf("sandbox timeout=%d, want explicit 300", got)
	}
	backend.cfg.Superserve.TimeoutSecs = 0
	backend.cfg.TTL = 0
	if got := backend.sandboxTimeoutSecs(); got != 5400 {
		t.Fatalf("sandbox timeout=%d, want safe fallback 5400", got)
	}
}

func TestConfigureReturnsLifecycleBackendAndCredentialedDoctor(t *testing.T) {
	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "")
	t.Setenv("SUPERSERVE_API_KEY", "")
	provider := Provider{}
	cfg := testConfig()
	rt := Runtime{Stdout: io.Discard, Stderr: io.Discard}
	configured, err := provider.Configure(cfg, rt)
	if err != nil {
		t.Fatalf("Configure err=%v", err)
	}
	delegated, ok := configured.(core.DelegatedRunBackend)
	if !ok {
		t.Fatalf("configured backend does not implement DelegatedRunBackend: %T", configured)
	}
	if _, err := delegated.Run(context.Background(), RunRequest{}); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("Run err=%v, want API key requirement", err)
	}
	cleanup, ok := configured.(core.CleanupBackend)
	if !ok {
		t.Fatalf("configured backend does not implement CleanupBackend: %T", configured)
	}
	_ = cleanup
	doctor, err := provider.ConfigureDoctor(cfg, rt)
	if err != nil {
		t.Fatalf("ConfigureDoctor err=%v", err)
	}
	if _, err := doctor.Doctor(context.Background(), DoctorRequest{}); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("Doctor err=%v, want API key requirement", err)
	}
}

func testConfig() Config {
	cfg := Config{}
	cfg.Provider = providerName
	cfg.Superserve.BaseURL = defaultBaseURL
	cfg.Superserve.Template = "superserve/base"
	cfg.Superserve.Workdir = defaultWorkdir
	cfg.Superserve.ExecTimeoutSecs = 600
	return cfg
}
