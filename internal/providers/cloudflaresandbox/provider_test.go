package cloudflaresandbox

import (
	"flag"
	"strings"
	"testing"

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
	if spec.Name != providerName || spec.Family != providerFamily {
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

func TestApplyFlagsUpdatesConfigWithoutTokenFlag(t *testing.T) {
	cfg := testConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("class", "", "")
	fs.String("type", "", "")
	values := (Provider{}).RegisterFlags(fs, cfg)
	if fs.Lookup("cloudflare-sandbox-token") != nil {
		t.Fatal("unexpected cloudflare-sandbox token flag")
	}
	if err := fs.Parse([]string{
		"--cloudflare-sandbox-url", "https://bridge.example.test/",
		"--cloudflare-sandbox-workdir", "/workspace/custom",
		"--cloudflare-sandbox-exec-timeout-secs", "120",
		"--cloudflare-sandbox-forget-missing",
	}); err != nil {
		t.Fatal(err)
	}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatalf("ApplyFlags err=%v", err)
	}
	if cfg.CloudflareSandbox.BridgeURL != "https://bridge.example.test/" || cfg.CloudflareSandbox.Workdir != "/workspace/custom" || cfg.CloudflareSandbox.ExecTimeoutSecs != 120 || !cfg.CloudflareSandbox.ForgetMissing {
		t.Fatalf("cfg=%#v", cfg.CloudflareSandbox)
	}
}

func TestApplyFlagsRejectsGenericSizing(t *testing.T) {
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
			if err == nil || !strings.Contains(err.Error(), "--"+flagName+" is not supported") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestValidateBridgeURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "https default port", raw: "HTTPS://BRIDGE.EXAMPLE.TEST:443/api/", want: "https://bridge.example.test/api"},
		{name: "loopback http", raw: "http://localhost:8787/", want: "http://localhost:8787"},
		{name: "IPv6 loopback", raw: "http://[::1]/", want: "http://[::1]"},
		{name: "missing", raw: "", wantErr: "requires cloudflareSandbox.url"},
		{name: "userinfo", raw: "https://user:pass@bridge.example.test", wantErr: "must not include userinfo"},
		{name: "query", raw: "https://bridge.example.test?token=secret", wantErr: "must not include query"},
		{name: "fragment", raw: "https://bridge.example.test/#secret", wantErr: "must not include query"},
		{name: "plain http", raw: "http://bridge.example.test", wantErr: "must use https"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.CloudflareSandbox.BridgeURL = tt.raw
			got, err := bridgeURL(cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v want %q", err, tt.wantErr)
				}
				if err != nil && strings.Contains(err.Error(), "pass") {
					t.Fatalf("error leaked credential: %v", err)
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

func TestValidateConfigRejectsBadValues(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "relative workdir", mutate: func(cfg *Config) { cfg.CloudflareSandbox.Workdir = "workspace" }, wantErr: "workdir must be absolute"},
		{name: "broad workdir", mutate: func(cfg *Config) { cfg.CloudflareSandbox.Workdir = "/workspace" }, wantErr: "too broad"},
		{name: "negative exec timeout", mutate: func(cfg *Config) { cfg.CloudflareSandbox.ExecTimeoutSecs = -1 }, wantErr: "execTimeoutSecs must be non-negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			tt.mutate(&cfg)
			err := validateProviderConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err=%v want %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigureReturnsRuntimeBackends(t *testing.T) {
	provider := Provider{}
	cfg := testConfig()
	configured, err := provider.Configure(cfg, Runtime{})
	if err != nil {
		t.Fatalf("Configure err=%v", err)
	}
	delegated, ok := configured.(core.DelegatedRunBackend)
	if !ok {
		t.Fatalf("configured backend does not implement DelegatedRunBackend: %T", configured)
	}
	if delegated == nil {
		t.Fatal("delegated backend nil")
	}
	cleanup, ok := configured.(core.CleanupBackend)
	if !ok {
		t.Fatalf("configured backend does not implement CleanupBackend: %T", configured)
	}
	if cleanup == nil {
		t.Fatal("cleanup backend nil")
	}
}

func testConfig() Config {
	cfg := Config{}
	cfg.CloudflareSandbox.BridgeURL = "https://bridge.example.test"
	cfg.CloudflareSandbox.Workdir = defaultWorkdir
	cfg.CloudflareSandbox.ExecTimeoutSecs = 600
	cfg.Provider = providerName
	return cfg
}
