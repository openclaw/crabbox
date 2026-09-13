package cloudflaresandbox

import (
	"flag"
	"net/http"
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
	for _, feature := range []core.Feature{core.FeatureArchiveSync, core.FeatureCleanup, core.FeatureRunSession} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
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

func TestCloudflareSandboxFlagPresenceAndValidationOrder(t *testing.T) {
	cfg := testConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterProviderFlags(fs, cfg)
	cfg.CloudflareSandbox = core.CloudflareSandboxConfig{BridgeURL: "https://example.invalid/prior", Workdir: "/workspace/prior", ExecTimeoutSecs: 12, ForgetMissing: true}
	before := cfg.CloudflareSandbox
	if err := ApplyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.CloudflareSandbox != before {
		t.Fatal("unvisited flags restored registration defaults")
	}
	if err := fs.Parse([]string{"--cloudflare-sandbox-url=", "--cloudflare-sandbox-workdir=", "--cloudflare-sandbox-exec-timeout-secs=0", "--cloudflare-sandbox-forget-missing=false"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyProviderFlags(&cfg, fs, values); err == nil || !strings.Contains(err.Error(), "requires cloudflareSandbox.url") {
		t.Fatalf("URL first=%v", err)
	}
	if cfg.CloudflareSandbox != (core.CloudflareSandboxConfig{}) {
		t.Fatal("all flags must apply before validation")
	}
	cfg.CloudflareSandbox = core.CloudflareSandboxConfig{BridgeURL: "https://example.invalid/api", Workdir: "relative", ExecTimeoutSecs: -1}
	if err := ApplyProviderFlags(&cfg, fs, struct{}{}); err != nil {
		t.Fatalf("foreign values reached validation: %v", err)
	}
	if err := validateProviderConfig(cfg); err == nil || err.Error() != "cloudflare-sandbox workdir must be absolute" {
		t.Fatalf("workdir before timeout=%v", err)
	}
	cfg.CloudflareSandbox.Workdir = "/workspace/app"
	if err := validateProviderConfig(cfg); err == nil || err.Error() != "cloudflare-sandbox execTimeoutSecs must be non-negative" {
		t.Fatalf("timeout validation=%v", err)
	}
	for _, name := range []string{"cloudflare-sandbox", " Cloudflare-Sandbox "} {
		cfg := testConfig()
		cfg.Provider = name
		fs := flag.NewFlagSet("guard", flag.ContinueOnError)
		fs.String("class", "", "")
		fs.String("type", "", "")
		values := RegisterProviderFlags(fs, cfg)
		for _, args := range [][]string{{"--type=vm"}, {"--type=vm", "--class=large"}} {
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			want := "--type"
			if len(args) == 2 {
				want = "--class"
			}
			for _, v := range []any{nil, struct{}{}, values} {
				before := cfg.CloudflareSandbox
				err := ApplyProviderFlags(&cfg, fs, v)
				if err == nil || err.Error() != want+" is not supported for provider=cloudflare-sandbox" {
					t.Fatalf("guard order=%v", err)
				}
				if cfg.CloudflareSandbox != before {
					t.Fatal("guard changed config")
				}
			}
		}
	}
}

func TestCloudflareSandboxOptionalTokenAndRawTimeout(t *testing.T) {
	for _, raw := range []string{"", "  ", " /workspace/app/ "} {
		cfg := testConfig()
		cfg.CloudflareSandbox.BridgeURL = "https://example.invalid/api"
		cfg.CloudflareSandbox.Token = ""
		cfg.CloudflareSandbox.Workdir = raw
		cfg.CloudflareSandbox.ExecTimeoutSecs = 0
		before := cfg.CloudflareSandbox
		if err := validateProviderConfig(cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := newBridgeClient(cfg, core.Runtime{HTTP: &http.Client{}}); err != nil {
			t.Fatalf("optional token=%v", err)
		}
		got, err := cloudflareSandboxWorkdir(cfg)
		want := "/workspace/crabbox"
		if strings.TrimSpace(raw) != "" {
			want = "/workspace/app"
		}
		if err != nil || got != want {
			t.Fatalf("workdir=%q error=%v", got, err)
		}
		b := backend{cfg: cfg}
		if b.execTimeoutSecs() != 0 {
			t.Fatal("explicit zero timeout filled from compiled default")
		}
		if cfg.CloudflareSandbox != before {
			t.Fatal("raw config changed")
		}
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
		mutate  func(*core.Config)
		wantErr string
	}{
		{name: "relative workdir", mutate: func(cfg *core.Config) { cfg.CloudflareSandbox.Workdir = "workspace" }, wantErr: "workdir must be absolute"},
		{name: "broad workdir", mutate: func(cfg *core.Config) { cfg.CloudflareSandbox.Workdir = "/workspace" }, wantErr: "too broad"},
		{name: "outside workspace workdir", mutate: func(cfg *core.Config) { cfg.CloudflareSandbox.Workdir = "/etc/crabbox" }, wantErr: "must be under /workspace/<dedicated-subdir>"},
		{name: "workspace traversal workdir", mutate: func(cfg *core.Config) { cfg.CloudflareSandbox.Workdir = "/workspace/../etc/crabbox" }, wantErr: "must be under /workspace/<dedicated-subdir>"},
		{name: "negative exec timeout", mutate: func(cfg *core.Config) { cfg.CloudflareSandbox.ExecTimeoutSecs = -1 }, wantErr: "execTimeoutSecs must be non-negative"},
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
	configured, err := provider.Configure(cfg, core.Runtime{})
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

func testConfig() core.Config {
	cfg := core.Config{}
	cfg.CloudflareSandbox.BridgeURL = "https://bridge.example.test"
	cfg.CloudflareSandbox.Workdir = "/workspace/crabbox"
	cfg.CloudflareSandbox.ExecTimeoutSecs = 600
	cfg.Provider = providerName
	return cfg
}
