package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression coverage for the two `crabbox stop` and static-lease UX gaps
// fixed in the same change: stop should accept --id like every other lease
// command, and ids that carry the synthesised `static_<slug>` prefix should
// route to provider=ssh without re-passing --provider / --static-host.

func TestAutoRouteStaticLeaseRestoresHostFromStaticClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	claimed := baseConfig()
	claimed.Provider = staticProvider
	claimed.Static.Host = "mac-studio.local"
	claimed.Static.User = "builder"
	claimed.Static.Port = "2202"
	claimed.Static.WorkRoot = "/Users/builder/project"
	claimed.TargetOS = targetMacOS
	if err := claimLeaseForRepoConfig("static_mac-studio-local", "mac-studio-local", claimed, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}

	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, nil); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := autoRouteStaticLease(&cfg, fs, "static_mac-studio-local"); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != staticProvider {
		t.Fatalf("provider=%q, want %q", cfg.Provider, staticProvider)
	}
	if cfg.Static.Host != "mac-studio.local" {
		t.Fatalf("static.host=%q, want mac-studio.local", cfg.Static.Host)
	}
	if cfg.Static.User != "builder" || cfg.Static.Port != "2202" || cfg.Static.WorkRoot != "/Users/builder/project" || cfg.TargetOS != targetMacOS {
		t.Fatalf("static route details not restored: %#v", cfg.Static)
	}
}

func TestAutoRouteStaticLeasePreservesRestoredDefaultsAfterProviderChange(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	claimed := baseConfig()
	claimed.Provider = staticProvider
	claimed.Static.Host = "builder.example.com"
	claimed.Static.User = "builder"
	claimed.Static.Port = "2202"
	claimed.Static.WorkRoot = "/srv/claimed"
	claimed.TargetOS = targetLinux
	if err := claimLeaseForRepoConfig("static_builder-example-com", "builder-example-com", claimed, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}

	cfg := baseConfig()
	cfg.Provider = "tart"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	fs := newFlagSet("test", io.Discard)
	registerTargetFlags(fs, cfg)
	if err := parseFlags(fs, nil); err != nil {
		t.Fatal(err)
	}
	if err := autoRouteStaticLease(&cfg, fs, "static_builder-example-com"); err != nil {
		t.Fatal(err)
	}
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != staticProvider || cfg.WorkRoot != "/srv/claimed" || cfg.SSHUser != "builder" || cfg.SSHPort != "2202" {
		t.Fatalf("routed static defaults changed: cfg=%#v static=%#v", cfg, cfg.Static)
	}
}

func TestAutoRouteStaticLeaseRestoresFriendlySlugClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	claimed := baseConfig()
	claimed.Provider = staticProvider
	claimed.Static.Host = "friendly.example.com"
	claimed.Static.User = "builder"
	claimed.Static.Port = "2202"
	claimed.Static.WorkRoot = "/work/friendly"
	claimed.TargetOS = targetMacOS
	if err := claimLeaseForRepoConfig("static_friendly-example-com", "friendly-slug", claimed, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}

	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, nil); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := autoRouteStaticLease(&cfg, fs, "friendly-slug"); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != staticProvider || cfg.Static.ID != "static_friendly-example-com" || cfg.Static.Name != "friendly-slug" {
		t.Fatalf("static identity not restored: provider=%q id=%q name=%q", cfg.Provider, cfg.Static.ID, cfg.Static.Name)
	}
	if cfg.Static.Host != "friendly.example.com" || cfg.Static.User != "builder" || cfg.Static.Port != "2202" || cfg.Static.WorkRoot != "/work/friendly" || cfg.TargetOS != targetMacOS {
		t.Fatalf("friendly static route details not restored: cfg=%#v static=%#v", cfg, cfg.Static)
	}
}

func TestAutoRouteStaticLeasePreservesAuthoritativeProvider(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	claimed := baseConfig()
	claimed.Provider = staticProvider
	claimed.Static.Host = "authoritative.example.com"
	claimed.Static.User = "builder"
	if err := claimLeaseForRepoConfig("static_authoritative", "authoritative-static", claimed, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fs := newFlagSet("authoritative static route", io.Discard)
	registerTargetFlags(fs, baseConfig())
	if err := parseFlags(fs, nil); err != nil {
		t.Fatal(err)
	}

	nonStatic := baseConfig()
	setProviderSelection(&nonStatic, "azure", providerSelectionRecordedRun)
	if err := autoRouteStaticLease(&nonStatic, fs, "authoritative-static"); err != nil {
		t.Fatal(err)
	}
	if nonStatic.Provider != "azure" || nonStatic.providerSelectionSource != providerSelectionRecordedRun || nonStatic.Static.Host != "" {
		t.Fatalf("non-static authoritative route changed: provider=%q source=%q static=%#v", nonStatic.Provider, nonStatic.providerSelectionSource, nonStatic.Static)
	}

	static := baseConfig()
	setProviderSelection(&static, staticProvider, providerSelectionRecordedRun)
	if err := autoRouteStaticLease(&static, fs, "authoritative-static"); err != nil {
		t.Fatal(err)
	}
	if static.Provider != staticProvider || static.providerSelectionSource != providerSelectionRecordedRun || static.Static.Host != claimed.Static.Host {
		t.Fatalf("static authoritative route=%q source=%q static=%#v", static.Provider, static.providerSelectionSource, static.Static)
	}
}

func TestLeaseTargetConfigPreservesImplicitStaticClaimRouting(t *testing.T) {
	isolateTestUserDirs(t)
	claimed := baseConfig()
	claimed.Provider = staticProvider
	claimed.Static.Host = "claimed-static.example.com"
	claimed.Static.User = "builder"
	claimed.Static.Port = "2202"
	claimed.Static.WorkRoot = "/work/claimed-static"
	claimed.TargetOS = targetMacOS
	if err := claimLeaseForRepoConfig("static_claimed-static", "claimed-static", claimed, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "crabbox.yaml")
	if err := os.WriteFile(configPath, []byte("provider: aws\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	defaults := defaultConfig()
	fs := newFlagSet("status", io.Discard)
	provider := fs.String("provider", defaults.Provider, "")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: "claimed-static"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != staticProvider || cfg.Static.Host != claimed.Static.Host || cfg.Static.User != claimed.Static.User || cfg.Static.Port != claimed.Static.Port || cfg.WorkRoot != claimed.Static.WorkRoot || cfg.TargetOS != targetMacOS {
		t.Fatalf("config=%#v static=%#v", cfg, cfg.Static)
	}
	if cfg.providerSelectionSource != providerSelectionLeaseContext {
		t.Fatalf("provider source=%q want %q", cfg.providerSelectionSource, providerSelectionLeaseContext)
	}
}

func TestLeaseTargetConfigProviderResourceIDSkipsStaticRouting(t *testing.T) {
	setupClaimRoutingCommandTest(t, parallelsProvider)
	claimed := baseConfig()
	claimed.Provider = staticProvider
	claimed.Static.Host = "builder.example.com"
	claimed.Static.User = "builder"
	claimed.Static.Port = "2202"
	claimed.Static.WorkRoot = "/work/static-builder"
	if err := claimLeaseForRepoConfig("static_builder", "builder", claimed, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}

	defaults := defaultConfig()
	if defaults.Provider != parallelsProvider || !providerSelectionIsActionable(defaults) {
		t.Fatalf("configured provider=%q source=%q", defaults.Provider, defaults.providerSelectionSource)
	}
	fs := newFlagSet("checkpoint list", io.Discard)
	provider := fs.String("provider", defaults.Provider, "")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{
		LeaseID:            "static_builder",
		ProviderResourceID: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != parallelsProvider || cfg.providerSelectionSource != defaults.providerSelectionSource {
		t.Fatalf("provider=%q source=%q want %q/%q", cfg.Provider, cfg.providerSelectionSource, parallelsProvider, defaults.providerSelectionSource)
	}
	if cfg.Static.ID != "" || cfg.Static.Name != "" || cfg.Static.Host != "" || cfg.Static.User != "" || cfg.Static.Port != "" || cfg.Static.WorkRoot != "" {
		t.Fatalf("provider-native id loaded Static config: %#v", cfg.Static)
	}
}

func TestAutoRouteStaticLeaseDoesNotGuessHostWithoutClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, nil); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := autoRouteStaticLease(&cfg, fs, "static_192-168-1-10"); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != staticProvider {
		t.Fatalf("provider=%q, want %q", cfg.Provider, staticProvider)
	}
	if cfg.Static.Host != "" {
		t.Fatalf("static.host=%q, want empty without claim/config", cfg.Static.Host)
	}
}

func TestAutoRouteStaticLeaseClaimOverridesConfiguredStaticHost(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	claimed := baseConfig()
	claimed.Provider = staticProvider
	claimed.Static.Host = "claimed.example.com"
	if err := claimLeaseForRepoConfig("static_claimed-example-com", "claimed-example-com", claimed, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}

	defaults := baseConfig()
	defaults.Static.Host = "default.example.com"
	fs := newFlagSet("test", io.Discard)
	registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, nil); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := autoRouteStaticLease(&cfg, fs, "static_claimed-example-com"); err != nil {
		t.Fatal(err)
	}
	if cfg.Static.Host != "claimed.example.com" {
		t.Fatalf("static.host=%q, want claimed.example.com", cfg.Static.Host)
	}
}

func TestAutoRouteStaticLeaseRespectsExplicitStaticTargetFlags(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	claimed := baseConfig()
	claimed.Provider = staticProvider
	claimed.Static.Host = "claimed.example.com"
	claimed.Static.User = "claimed"
	claimed.Static.Port = "2222"
	claimed.Static.WorkRoot = "/claimed"
	claimed.TargetOS = targetMacOS
	if err := claimLeaseForRepoConfig("static_claimed-example-com", "claimed-example-com", claimed, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}

	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	target := registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--static-host", "flag.example.com",
		"--static-user", "flag-user",
		"--static-port", "2022",
		"--static-work-root", "/flag",
		"--target", "linux",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := applyTargetFlagOverrides(&cfg, fs, target); err != nil {
		t.Fatal(err)
	}
	if err := autoRouteStaticLease(&cfg, fs, "static_claimed-example-com"); err != nil {
		t.Fatal(err)
	}
	if cfg.Static.Host != "flag.example.com" || cfg.Static.User != "flag-user" || cfg.Static.Port != "2022" || cfg.Static.WorkRoot != "/flag" || cfg.TargetOS != targetLinux {
		t.Fatalf("explicit static target flags not preserved: cfg=%#v static=%#v", cfg, cfg.Static)
	}
}

func TestAutoRouteStaticLeaseRespectsExplicitProviderFlag(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	provider := fs.String("provider", defaults.Provider, "")
	registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, []string{"--provider", "hetzner"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	cfg.Provider = *provider
	if err := autoRouteStaticLease(&cfg, fs, "static_my-box"); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "hetzner" {
		t.Fatalf("provider=%q, want hetzner (user override)", cfg.Provider)
	}
	if cfg.Static.Host != "" {
		t.Fatalf("static.host=%q, want empty (non-ssh provider)", cfg.Static.Host)
	}
}

func TestAutoRouteStaticLeaseSkipsClaimScanForExplicitNonStaticProvider(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir, err := crabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	claimsDir := filepath.Join(dir, "claims")
	if err := os.MkdirAll(claimsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claimsDir, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	provider := fs.String("provider", defaults.Provider, "")
	registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, []string{"--provider", "hetzner"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	cfg.Provider = *provider
	if err := autoRouteStaticLease(&cfg, fs, "friendly-slug"); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "hetzner" {
		t.Fatalf("provider=%q, want hetzner", cfg.Provider)
	}
}

func TestAutoRouteStaticLeaseRespectsExplicitStaticHost(t *testing.T) {
	isolateTestUserDirs(t)
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	target := registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, []string{"--static-host", "other-box"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := applyTargetFlagOverrides(&cfg, fs, target); err != nil {
		t.Fatal(err)
	}
	if err := autoRouteStaticLease(&cfg, fs, "static_my-box"); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != staticProvider {
		t.Fatalf("provider=%q, want %q", cfg.Provider, staticProvider)
	}
	if cfg.Static.Host != "other-box" {
		t.Fatalf("static.host=%q, want other-box (user override)", cfg.Static.Host)
	}
}

func TestAutoRouteStaticLeaseIgnoresNonStaticIDs(t *testing.T) {
	isolateTestUserDirs(t)
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, nil); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := autoRouteStaticLease(&cfg, fs, "cbx_abc123"); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != defaults.Provider {
		t.Fatalf("provider=%q, want %q (unchanged)", cfg.Provider, defaults.Provider)
	}
	if cfg.Static.Host != "" {
		t.Fatalf("static.host=%q, want empty (unchanged)", cfg.Static.Host)
	}
}

// Issue 1 end-to-end: applyLeaseCreateFlagsForLease (the path `crabbox run`
// uses) must auto-route static_<slug> ids so users don't have to repeat
// --provider ssh --static-host on every command after warmup.
func TestApplyLeaseCreateFlagsForLeaseAutoRoutesStaticID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	claimed := baseConfig()
	claimed.Provider = staticProvider
	claimed.Static.Host = "dev.example.com"
	if err := claimLeaseForRepoConfig("static_dev-example-com", "dev-example-com", claimed, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}

	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaults)
	// run.go owns its own --id flag outside the lease-create flag set; the
	// existing lease id is then handed to applyLeaseCreateFlagsForLease as a
	// plain string. Mirror that here.
	if err := parseFlags(fs, nil); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := applyLeaseCreateFlagsForLease(&cfg, fs, values, "static_dev-example-com"); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != staticProvider {
		t.Fatalf("provider=%q, want %q", cfg.Provider, staticProvider)
	}
	if cfg.Static.Host != "dev.example.com" {
		t.Fatalf("static.host=%q, want dev.example.com", cfg.Static.Host)
	}
}

// Issue 2: runStopCommand must emit `--id <lease>` so the emitted command can
// be pasted back into `crabbox stop` (which now accepts --id like every other
// lease command).
func TestRunStopCommandEmitsIDFlag(t *testing.T) {
	got := runStopCommand(Config{Provider: "aws", TargetOS: targetLinux}, "cbx_123")
	want := "--id cbx_123"
	if !strings.Contains(got, want) {
		t.Fatalf("stop command missing %q:\n%s", want, got)
	}
	if strings.Contains(got, " cbx_123") && !strings.Contains(got, "--id cbx_123") {
		t.Fatalf("stop command should not emit lease id as trailing positional:\n%s", got)
	}
}

func TestStopAcceptsIDFlag(t *testing.T) {
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).stop(context.Background(), []string{"--provider", "e2b", "--id", "box_123"})
	if err != nil {
		t.Fatalf("stop --id: %v", err)
	}
}

func TestStopRejectsIDFlagWithExtraPositional(t *testing.T) {
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).stop(context.Background(), []string{"--provider", "e2b", "--id", "box_123", "box_456"})
	if err == nil || !strings.Contains(err.Error(), "usage: crabbox stop --id") {
		t.Fatalf("expected usage error for --id plus positional, got %v", err)
	}
}

func TestStopRejectsReclaimForProviderWithoutAdoptionContract(t *testing.T) {
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).stop(context.Background(), []string{
		"--provider", "e2b", "--id", "box_123", "--reclaim",
	})
	if err == nil || !strings.Contains(err.Error(), "does not support stop --reclaim") {
		t.Fatalf("stop --reclaim err=%v, want unsupported-provider rejection", err)
	}
}

func TestStopDispatchesExplicitReclaimContract(t *testing.T) {
	called := false
	testStopReclaimHook = func(req StopRequest) error {
		called = true
		if req.ID != "service-123" {
			t.Fatalf("reclaim id=%q", req.ID)
		}
		return nil
	}
	t.Cleanup(func() { testStopReclaimHook = nil })

	err := (App{Stdout: io.Discard, Stderr: io.Discard}).stop(context.Background(), []string{
		"--provider", "stop-reclaim-test", "--id", "service-123", "--reclaim",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("stop did not dispatch the explicit reclaim contract")
	}
}

func TestStopForceRequiresExplicitProviderAndResourceID(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "provider", args: []string{"--id", "service-123", "--force"}, want: "requires an explicit --provider"},
		{name: "resource", args: []string{"--provider", "stop-reclaim-test", "--force", "service-123"}, want: "requires an exact --id"},
		{name: "reclaim conflict", args: []string{"--provider", "stop-reclaim-test", "--id", "service-123", "--force", "--reclaim"}, want: "cannot be combined with --reclaim"},
		{name: "expected identity conflict", args: []string{"--provider", "stop-reclaim-test", "--id", "service-123", "--force", "--expected-provider-resource-id", "replacement-resource"}, want: "cannot be combined with controller-owned release identity"},
		{name: "confirmed absence conflict", args: []string{"--provider", "stop-reclaim-test", "--id", "service-123", "--force", "--confirmed-absent-local-cleanup"}, want: "cannot be combined with controller-owned release identity"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := (App{Stdout: io.Discard, Stderr: io.Discard}).stop(context.Background(), test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("stop --force err=%v, want %q", err, test.want)
			}
		})
	}
}

func TestStopForceDispatchesVerifiedReclaimContract(t *testing.T) {
	called := false
	testStopReclaimHook = func(req StopRequest) error {
		called = true
		if req.ID != "service-123" {
			t.Fatalf("forced recovery id=%q", req.ID)
		}
		return nil
	}
	t.Cleanup(func() { testStopReclaimHook = nil })

	err := (App{Stdout: io.Discard, Stderr: io.Discard}).stop(context.Background(), []string{
		"--provider", "stop-reclaim-test", "--id", "service-123", "--force",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("stop --force did not dispatch the verified reclaim contract")
	}
}

func TestStopForceRejectsControllerIdentityBeforeVerifiedAdoption(t *testing.T) {
	called := false
	testStopReclaimHook = func(StopRequest) error {
		called = true
		return nil
	}
	t.Cleanup(func() { testStopReclaimHook = nil })

	err := (App{Stdout: io.Discard, Stderr: io.Discard}).stop(context.Background(), []string{
		"--provider", "stop-reclaim-test", "--id", "service-123", "--force",
		"--expected-provider-lease-id", "cbx_abcdef123456",
		"--expected-provider-attempt-lease-id", "cbx_abcdef123456",
		"--expected-provider-slug", "original-resource",
		"--expected-provider-resource-id", "original-resource",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined with controller-owned release identity") {
		t.Fatalf("stop --force err=%v, want expected-identity rejection", err)
	}
	if called {
		t.Fatal("stop --force dispatched adoption before rejecting controller identity")
	}
}

func TestStopForceRejectsProviderWithoutVerifiedRecovery(t *testing.T) {
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).stop(context.Background(), []string{
		"--provider", "e2b", "--id", "box_123", "--force",
	})
	if err == nil || !strings.Contains(err.Error(), "does not support verified forced recovery") {
		t.Fatalf("stop --force err=%v, want unsupported-provider rejection", err)
	}
}

func TestStopForceRejectsSSHProviderWithoutVerifiedRecovery(t *testing.T) {
	isolateTestUserDirs(t)
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).stop(context.Background(), []string{
		"--provider", "ssh", "--static-host", "host.example.test", "--id", "static_host-example-test", "--force",
	})
	if err == nil || !strings.Contains(err.Error(), "does not support verified forced recovery") {
		t.Fatalf("stop --force err=%v, want SSH-provider rejection", err)
	}
}

func TestStopRejectsReclaimForSSHLeaseProvider(t *testing.T) {
	isolateTestUserDirs(t)
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).stop(context.Background(), []string{
		"--provider", "ssh", "--static-host", "host.example.test", "--id", "static_host-example-test", "--reclaim",
	})
	if err == nil || !strings.Contains(err.Error(), "does not support stop --reclaim") {
		t.Fatalf("stop --reclaim err=%v, want SSH-provider rejection", err)
	}
}
