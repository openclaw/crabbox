package cli

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tailscaleDefaultsTestProvider struct {
	testAWSProvider
	name          string
	supplyDefault bool
}

func (p tailscaleDefaultsTestProvider) Name() string { return p.name }

func (p tailscaleDefaultsTestProvider) Spec() ProviderSpec {
	spec := p.testAWSProvider.Spec()
	spec.Name = p.name
	spec.Targets = []TargetSpec{{OS: targetLinux}}
	spec.Features = append(spec.Features, FeatureTailscale)
	return spec
}

func (p tailscaleDefaultsTestProvider) ApplyConfigDefaults(cfg *Config) error {
	if p.supplyDefault {
		ApplyTailscaleEnabledDefault(cfg, true)
	}
	return nil
}

func TestProviderTailscaleDefaultsAcrossSelection(t *testing.T) {
	const source = "test-tailscale-default"
	const destination = "test-no-tailscale-default"
	for _, provider := range []tailscaleDefaultsTestProvider{
		{name: source, supplyDefault: true},
		{name: destination},
	} {
		name := provider.Name()
		previous, existed := providerRegistry[name]
		providerRegistry[name] = provider
		t.Cleanup(func() {
			if existed {
				providerRegistry[name] = previous
			} else {
				delete(providerRegistry, name)
			}
		})
	}
	for _, tc := range []struct {
		name      string
		args      []string
		want      bool
		wantReset bool
	}{
		{name: "omitted", want: true},
		{name: "explicit false", args: []string{"--tailscale=false"}},
		{name: "explicit true", args: []string{"--tailscale=true"}, want: true, wantReset: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Provider = source
			if err := applyProviderConfigDefaults(&cfg); err != nil {
				t.Fatal(err)
			}
			fs := flag.NewFlagSet(tc.name, flag.ContinueOnError)
			values := registerLeaseCreateFlags(fs, baseConfig())
			if err := fs.Parse(append([]string{"--provider", source}, tc.args...)); err != nil {
				t.Fatal(err)
			}
			if err := applyLeaseCreateFlagsForLeaseMode(&cfg, fs, values, "", false); err != nil {
				t.Fatal(err)
			}
			if err := applyProviderConfigDefaults(&cfg); err != nil {
				t.Fatal(err)
			}
			var body struct {
				Tailscale *bool `json:"tailscale"`
			}
			client := CoordinatorClient{
				BaseURL: "https://coordinator.example.test",
				Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.Method != http.MethodPost || r.URL.Path != "/v1/leases" {
						t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"lease":{"id":"cbx_123456abcdef"}}`))}, nil
				})},
			}
			if _, err := client.CreateLease(context.Background(), cfg, "ssh-ed25519 test", true, "", "test"); err != nil {
				t.Fatal(err)
			}
			if body.Tailscale == nil || *body.Tailscale != tc.want {
				t.Fatalf("serialized enabled=%v, want %v", body.Tailscale, tc.want)
			}
			cfg.Provider = destination
			if err := applyProviderConfigDefaults(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Tailscale.Enabled != tc.wantReset {
				t.Fatalf("after provider change=%v, want %v", cfg.Tailscale.Enabled, tc.wantReset)
			}
			cfg.Provider = source
			if err := applyProviderConfigDefaults(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Tailscale.Enabled != tc.want {
				t.Fatalf("after reselect=%v, want %v", cfg.Tailscale.Enabled, tc.want)
			}
		})
	}
}

func TestProviderTailscaleDefaultsPreserveIntent(t *testing.T) {
	for _, tc := range []struct {
		name         string
		initial      bool
		explicit     bool
		defaultValue bool
		override     bool
		want         bool
		wantReset    bool
	}{
		{name: "omitted", defaultValue: true, want: true},
		{name: "explicit false", explicit: true, defaultValue: true},
		{name: "explicit true", initial: true, explicit: true, defaultValue: true, want: true, wantReset: true},
		{name: "programmatic true", initial: true, defaultValue: true, want: true, wantReset: true},
		{name: "programmatic true beats false default", initial: true, want: true, wantReset: true},
		{name: "programmatic false after default", defaultValue: true, override: true},
		{name: "programmatic true after false default", override: true, want: true, wantReset: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Tailscale.Enabled = tc.initial
			if tc.explicit {
				MarkTailscaleEnabledExplicit(&cfg)
			}
			ApplyTailscaleEnabledDefault(&cfg, tc.defaultValue)
			if tc.override {
				cfg.Tailscale.Enabled = !tc.defaultValue
			}
			ApplyTailscaleEnabledDefault(&cfg, tc.defaultValue)
			if cfg.Tailscale.Enabled != tc.want {
				t.Fatalf("enabled=%v, want %v", cfg.Tailscale.Enabled, tc.want)
			}
			copied := cfg
			resetProviderDerivedDefaults(&copied)
			if copied.Tailscale.Enabled != tc.wantReset {
				t.Fatalf("after provider reset=%v, want %v", copied.Tailscale.Enabled, tc.wantReset)
			}
			ApplyTailscaleEnabledDefault(&copied, tc.defaultValue)
			if copied.Tailscale.Enabled != tc.want {
				t.Fatalf("after reselect=%v, want %v", copied.Tailscale.Enabled, tc.want)
			}
		})
	}
}

func TestProviderTailscaleDefaultsRespectConfigSources(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		env  string
		args []string
		want bool
	}{
		{name: "omitted", file: "tailscale: {}\n", want: true},
		{name: "null", file: "tailscale: {enabled: null}\n", want: true},
		{name: "yaml false", file: "tailscale: {enabled: false}\n"},
		{name: "yaml true", file: "tailscale: {enabled: true}\n", want: true},
		{name: "env false", env: "false"},
		{name: "env true overrides file", file: "tailscale: {enabled: false}\n", env: "true", want: true},
		{name: "flag false", args: []string{"--tailscale=false"}},
		{name: "flag true overrides env", env: "false", args: []string{"--tailscale=true"}, want: true},
		{name: "file env flag precedence", file: "tailscale: {enabled: false}\n", env: "true", args: []string{"--tailscale=false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			ApplyTailscaleEnabledDefault(&cfg, true)
			if tc.file != "" {
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := applyConfigFile(&cfg, path, configPathTrust{}); err != nil {
					t.Fatal(err)
				}
				ApplyTailscaleEnabledDefault(&cfg, true)
			}
			if tc.env != "" {
				t.Setenv("CRABBOX_TAILSCALE", tc.env)
				if err := applyEnv(&cfg); err != nil {
					t.Fatal(err)
				}
				ApplyTailscaleEnabledDefault(&cfg, true)
			}
			if tc.args != nil {
				fs := flag.NewFlagSet(tc.name, flag.ContinueOnError)
				values := registerNetworkFlags(fs, baseConfig())
				if err := fs.Parse(tc.args); err != nil {
					t.Fatal(err)
				}
				if err := applyNetworkFlagOverrides(&cfg, fs, values); err != nil {
					t.Fatal(err)
				}
			}
			ApplyTailscaleEnabledDefault(&cfg, true)
			if cfg.Tailscale.Enabled != tc.want {
				t.Fatalf("enabled=%v, want %v", cfg.Tailscale.Enabled, tc.want)
			}
		})
	}
}

func TestNetworkPublicIgnoresTailscaleMetadata(t *testing.T) {
	cfg := baseConfig()
	cfg.Network = NetworkPublic
	server := Server{Labels: map[string]string{
		"lease":          "cbx_abcdef123456",
		"tailscale":      "true",
		"tailscale_fqdn": "crabbox-blue.example.ts.net",
	}}
	target := SSHTarget{Host: "203.0.113.10", Port: "2222"}
	got, err := resolveNetworkTarget(context.Background(), cfg, server, target)
	if err != nil {
		t.Fatal(err)
	}
	if got.Network != NetworkPublic || got.Target.Host != "203.0.113.10" {
		t.Fatalf("resolve public = network=%s host=%s", got.Network, got.Target.Host)
	}
}

func TestNetworkTailscaleRequiresMetadata(t *testing.T) {
	cfg := baseConfig()
	cfg.Network = NetworkTailscale
	_, err := resolveNetworkTarget(context.Background(), cfg, Server{Labels: map[string]string{"lease": "cbx_abcdef123456"}}, SSHTarget{Host: "203.0.113.10"})
	if err == nil {
		t.Fatal("expected network=tailscale without metadata to fail")
	}
}

func TestLoginOnlySSHConfigProxyIgnoresInboundTailscaleSelection(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "islo"
	cfg.Network = NetworkTailscale
	server := Server{Labels: map[string]string{
		"lease":          "isb_crabbox-repo-abcdef",
		"tailscale":      "true",
		"tailscale_fqdn": "outbound-only.example.ts.net",
	}}
	target := SSHTarget{Host: "crabbox-repo-abcdef.islo", Port: "22", SSHConfigProxy: true}
	got, err := resolveSSHTargetNetwork(context.Background(), cfg, server, target, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Target.Host != target.Host || !got.Target.SSHConfigProxy {
		t.Fatalf("login proxy target=%#v", got.Target)
	}
}

func TestSSHConfigProxyStillHonorsInboundTailscaleSelection(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.Network = NetworkTailscale
	target := SSHTarget{Host: "proxy.example", Port: "22", SSHConfigProxy: true}
	if _, err := resolveSSHTargetNetwork(context.Background(), cfg, Server{}, target, true); err == nil {
		t.Fatal("expected non-egress-only proxy target to require tailnet metadata")
	}
}

func TestBootstrapNetworkPrefersTailscaleForExitNode(t *testing.T) {
	cfg := baseConfig()
	cfg.Network = NetworkAuto
	server := Server{
		Labels: map[string]string{
			"tailscale":           "true",
			"tailscale_hostname":  "crabbox-blue-lobster",
			"tailscale_exit_node": "100.123.224.76",
		},
	}
	server.PublicNet.IPv4.IP = "203.0.113.10"
	target := SSHTarget{Host: "203.0.113.10", Port: "2222"}
	got := bootstrapNetworkTarget(cfg, server, target)
	if got.Host != "crabbox-blue-lobster" || got.NetworkKind != NetworkTailscale {
		t.Fatalf("bootstrap target = host=%s network=%s", got.Host, got.NetworkKind)
	}
}

func TestBootstrapNetworkHonorsExplicitPublic(t *testing.T) {
	cfg := baseConfig()
	cfg.Network = NetworkPublic
	server := Server{Labels: map[string]string{
		"tailscale":           "true",
		"tailscale_hostname":  "crabbox-blue-lobster",
		"tailscale_exit_node": "100.123.224.76",
	}}
	target := SSHTarget{Host: "203.0.113.10", Port: "2222"}
	got := bootstrapNetworkTarget(cfg, server, target)
	if got.Host != "203.0.113.10" || got.NetworkKind != "" {
		t.Fatalf("bootstrap target = host=%s network=%s", got.Host, got.NetworkKind)
	}
}

func TestTailscaleExitNodeEgressCheckFailsClosed(t *testing.T) {
	script := tailscaleExitNodeEgressCheckScript()
	for _, want := range []string{
		"command -v tailscale",
		"tailscale debug prefs",
		"tailscale prefs unavailable",
		"tailscale prefs did not include ExitNodeID",
		"exit node is not selected",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("egress check script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "debug prefs 2>/dev/null || true") {
		t.Fatalf("egress check script must not ignore tailscale prefs failures:\n%s", script)
	}
}

func TestRenderTailscaleHostname(t *testing.T) {
	got := renderTailscaleHostname("CBX-{slug}-{provider}-{id}", "cbx_abcdef123456", "Blue Lobster", "aws")
	if got != "cbx-blue-lobster-aws-cbx-abcdef123456" {
		t.Fatalf("renderTailscaleHostname=%q", got)
	}
}

func TestCoordinatorTailscaleProviderMismatchRetainsPreviousServer(t *testing.T) {
	previous := Server{CloudID: "i-original", Provider: "aws", Name: "original"}
	updated, err := coordinatorTailscaleResponseServer(Config{Provider: "aws"}, previous, CoordinatorLease{
		ID: "cbx_tailscale_identity", Provider: "external", CloudID: "external-workspace", ServerName: "replacement",
	})
	if !isCoordinatorProviderIdentityError(err) {
		t.Fatalf("error=%v, want typed provider identity mismatch", err)
	}
	if updated.CloudID != previous.CloudID || updated.Provider != previous.Provider || updated.Name != previous.Name {
		t.Fatalf("updated server=%#v, want previous=%#v", updated, previous)
	}
}

func TestValidateNetworkConfigRejectsStaticProvisioning(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "ssh"
	cfg.Tailscale.Enabled = true
	if err := validateNetworkConfig(cfg); err == nil {
		t.Fatal("expected --tailscale static provider validation failure")
	}
}
