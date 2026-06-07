package cli

import (
	"context"
	"io"
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

func TestAutoRouteStaticLeaseRespectsExplicitStaticHost(t *testing.T) {
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
