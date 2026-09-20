package superserve

import (
	"context"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecIsDelegatedLinuxAndAliasFree(t *testing.T) {
	provider := Provider{}
	if provider.Spec().Name != providerName {
		t.Fatalf("Name=%q want %q", provider.Spec().Name, providerName)
	}
	if aliases := provider.Spec().Aliases; len(aliases) != 0 {
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
	if got.Spec().Name != providerName {
		t.Fatalf("ProviderFor(superserve).Name=%q", got.Spec().Name)
	}
	for _, alias := range []string{"ss", "sup", "super-serve"} {
		if got, err := core.ProviderFor(alias); err == nil && got.Spec().Name == providerName {
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
		{name: "whitespace default", raw: " \t ", want: defaultBaseURL},
		{name: "empty query marker", raw: "https://EXAMPLE.test/api?", want: "https://example.test/api?"},
		{name: "escaped slash", raw: "https://EXAMPLE.test/a%2Fb", want: "https://example.test/a%2Fb"},
		{name: "escaped slash trailing slash", raw: "https://EXAMPLE.test/a%2Fb/", want: "https://example.test/a/b"},
		{name: "custom IPv6 port", raw: "https://[2001:DB8::1]:8443/api///", want: "https://[2001:db8::1]:8443/api"},
		{name: "components before scheme", raw: "ftp://user@example.test/api", wantErr: "must not contain userinfo"},
		{name: "absolute before components", raw: "//user@example.test/api", wantErr: "must be an absolute URL"},
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
		mutate  func(*core.Config)
		wantErr string
	}{
		{name: "relative workdir", mutate: func(cfg *core.Config) { cfg.Superserve.Workdir = "workspace" }, wantErr: "workdir must be absolute"},
		{name: "broad workdir", mutate: func(cfg *core.Config) { cfg.Superserve.Workdir = "/workspace" }, wantErr: "too broad"},
		{name: "system workdir", mutate: func(cfg *core.Config) { cfg.Superserve.Workdir = "/etc" }, wantErr: "too broad"},
		{name: "negative timeout", mutate: func(cfg *core.Config) { cfg.Superserve.TimeoutSecs = -1 }, wantErr: "timeoutSecs must be non-negative"},
		{name: "negative exec timeout", mutate: func(cfg *core.Config) { cfg.Superserve.ExecTimeoutSecs = -1 }, wantErr: "execTimeoutSecs must be non-negative"},
		{name: "timeout over seven days", mutate: func(cfg *core.Config) { cfg.Superserve.TimeoutSecs = maxSuperserveSandboxTimeoutSecs + 1 }, wantErr: "must not exceed 604800"},
		{name: "derived TTL over seven days", mutate: func(cfg *core.Config) { cfg.TTL = 7*24*time.Hour + time.Millisecond }, wantErr: "must not exceed 604800"},
		{name: "deny hostname", mutate: func(cfg *core.Config) { cfg.Superserve.NetworkDenyOut = []string{"metadata.example.test"} }, wantErr: "must be a CIDR"},
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
	backend := NewSuperserveBackend((Provider{}).Spec(), cfg, core.Runtime{}).(*backend)
	if got := backend.execTimeoutSecs(); got != 0 {
		t.Fatalf("exec timeout=%d, want service default marker 0", got)
	}
}

func TestSuperserveSandboxTimeoutUsesConfiguredValueOrTTL(t *testing.T) {
	cfg := testConfig()
	cfg.TTL = 2*time.Minute + time.Millisecond
	backend := NewSuperserveBackend((Provider{}).Spec(), cfg, core.Runtime{}).(*backend)
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
	rt := core.Runtime{Stdout: io.Discard, Stderr: io.Discard}
	configured, err := provider.Configure(cfg, rt)
	if err != nil {
		t.Fatalf("Configure err=%v", err)
	}
	delegated, ok := configured.(core.DelegatedRunBackend)
	if !ok {
		t.Fatalf("configured backend does not implement DelegatedRunBackend: %T", configured)
	}
	if _, err := delegated.Run(context.Background(), core.RunRequest{}); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("Run err=%v, want API key requirement", err)
	}
	cleanup, ok := configured.(core.CleanupBackend)
	if !ok {
		t.Fatalf("configured backend does not implement CleanupBackend: %T", configured)
	}
	_ = cleanup
	doctor, err := core.ConfigureProviderDoctor(provider, cfg, rt)
	if err != nil {
		t.Fatalf("ConfigureDoctor err=%v", err)
	}
	if _, err := doctor.Doctor(context.Background(), core.DoctorRequest{}); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("Doctor err=%v, want API key requirement", err)
	}
}

func testConfig() core.Config {
	cfg := core.Config{}
	cfg.Provider = providerName
	cfg.Superserve.BaseURL = defaultBaseURL
	cfg.Superserve.Template = "superserve/base"
	cfg.Superserve.Workdir = defaultWorkdir
	cfg.Superserve.ExecTimeoutSecs = 600
	return cfg
}

func TestSuperserveListFlagValueContract(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{nil, []string{" prior "}},
		{[]string{"--superserve-network-allow-out="}, []string{}},
		{[]string{"--superserve-network-allow-out=old", "--superserve-network-allow-out= a, ,a, none "}, []string{"a", "a", "none"}},
	} {
		cfg := testConfig()
		prior := []string{" prior "}
		cfg.Superserve.NetworkAllowOut = prior
		cfg.Superserve.NetworkDenyOut = []string{"192.0.2.0/24"}
		fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
		values := (Provider{}).RegisterFlags(fs, cfg)
		if fs.Lookup("superserve-network-allow-out").DefValue != " prior " {
			t.Fatal("registration changed raw defaults")
		}
		if err := fs.Parse(tc.args); err != nil {
			t.Fatal(err)
		}
		if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cfg.Superserve.NetworkAllowOut, tc.want) || !reflect.DeepEqual(cfg.Superserve.NetworkDenyOut, []string{"192.0.2.0/24"}) {
			t.Fatalf("lists=%#v", cfg.Superserve)
		}
		if prior[0] != " prior " {
			t.Fatal("flags mutated inherited storage")
		}
	}
}

func TestSuperserveBindingFlagValidationPhases(t *testing.T) {
	for _, provider := range []string{providerName, " SuPeRsErVe ", "fixture-other"} {
		for _, generic := range []string{"class", "type"} {
			cfg := testConfig()
			cfg.Provider = provider
			cfg.Superserve.TimeoutSecs = -1
			before := cfg
			fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
			fs.String(generic, "", "")
			values := (Provider{}).RegisterFlags(fs, cfg)
			if err := (Provider{}).ApplyFlags(&cfg, fs, struct{}{}); err != nil || !reflect.DeepEqual(cfg, before) {
				t.Fatal("foreign values gained validation or mutation")
			}
			if err := (Provider{}).ApplyFlags(&cfg, fs, values); err == nil || !strings.Contains(err.Error(), "timeoutSecs") || !reflect.DeepEqual(cfg, before) {
				t.Fatal("typed unvisited values skipped final validation")
			}
			if err := fs.Set(generic, "fixture"); err != nil {
				t.Fatal(err)
			}
			err := (Provider{}).ApplyFlags(&cfg, fs, struct{}{})
			if provider == "fixture-other" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "--"+generic) {
				t.Fatal("selected sizing guard moved after type assertion")
			}
		}
		cfg := testConfig()
		cfg.Provider = provider
		fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
		values := (Provider{}).RegisterFlags(fs, cfg)
		if err := fs.Parse([]string{"--superserve-template=fixture", "--superserve-timeout-secs=-1", "--superserve-exec-timeout-secs=-2", "--superserve-network-allow-out= a, ,a ", "--superserve-network-deny-out=192.0.2.0/24", "--superserve-forget-missing=true"}); err != nil {
			t.Fatal(err)
		}
		want := cfg
		want.Superserve.Template, want.Superserve.TimeoutSecs, want.Superserve.ExecTimeoutSecs = "fixture", -1, -2
		want.Superserve.NetworkAllowOut, want.Superserve.NetworkDenyOut, want.Superserve.ForgetMissing = []string{"a", "a"}, []string{"192.0.2.0/24"}, true
		core.RecordProviderFlagInputs(&want, true, "superserve")
		err := (Provider{}).ApplyFlags(&cfg, fs, values)
		if err == nil || !strings.Contains(err.Error(), "timeoutSecs") || !reflect.DeepEqual(cfg, want) {
			t.Fatal("final validation moved before later assignments or accepted facts")
		}
	}
}

func TestSuperserveBindingDenyListFlags(t *testing.T) {
	for _, raw := range []string{"", "  ", " , ", " 192.0.2.0/24, ,192.0.2.0/24 "} {
		cfg := testConfig()
		cfg.Superserve.NetworkDenyOut = []string{"198.51.100.0/24"}
		fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
		values := (Provider{}).RegisterFlags(fs, cfg)
		if err := fs.Parse([]string{"--superserve-network-deny-out=203.0.113.0/24", "--superserve-network-deny-out=" + raw}); err != nil {
			t.Fatal(err)
		}
		want := cfg
		want.Superserve.NetworkDenyOut = []string{}
		if strings.Contains(raw, "192.") {
			want.Superserve.NetworkDenyOut = []string{"192.0.2.0/24", "192.0.2.0/24"}
		}
		core.RecordProviderFlagInputs(&want, true, "superserve")
		for repeat := 0; repeat < 2; repeat++ {
			if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil || !reflect.DeepEqual(cfg, want) {
				t.Fatalf("deny scalar flag contract changed: %v", err)
			}
		}
	}
}
