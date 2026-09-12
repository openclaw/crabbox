package blaxel

import (
	"flag"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecAndRegistration(t *testing.T) {
	p := Provider{}
	if p.Name() != providerName {
		t.Fatalf("Name=%q want %q", p.Name(), providerName)
	}
	if len(p.Aliases()) != 0 {
		t.Fatalf("Aliases=%v, want none", p.Aliases())
	}
	spec := p.Spec()
	if spec.Name != providerName || spec.Family != providerName {
		t.Fatalf("spec identity=%#v", spec)
	}
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("Kind=%q want delegated-run", spec.Kind)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("Targets=%#v", spec.Targets)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) || !spec.Features.Has(core.FeatureCleanup) || !spec.Features.Has(core.FeatureRunSession) {
		t.Fatalf("Features=%#v, want archive-sync, cleanup, and run-session", spec.Features)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("Coordinator=%q want never", spec.Coordinator)
	}
	got, err := core.ProviderFor(providerName)
	if err != nil {
		t.Fatalf("ProviderFor(blaxel): %v", err)
	}
	if got.Name() != providerName {
		t.Fatalf("ProviderFor(blaxel).Name=%q", got.Name())
	}
	for _, alias := range []string{"blx", "sandbox"} {
		if got, err := core.ProviderFor(alias); err == nil && got.Name() == providerName {
			t.Fatalf("alias %q unexpectedly resolves to blaxel", alias)
		}
	}
}

func TestProviderFlagsApplyAndValidate(t *testing.T) {
	cfg := core.Config{Provider: providerName, Blaxel: core.BlaxelConfig{APIURL: "https://api.blaxel.ai", Workdir: "/workspace/crabbox"}}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := Provider{}.RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--blaxel-api-url", "https://API.BLAXEL.AI:443/v1/",
		"--blaxel-workspace", "workspace-test",
		"--blaxel-region", "us-pdx-1",
		"--blaxel-image", "ubuntu:24.04",
		"--blaxel-memory-mb", "2048",
		"--blaxel-ttl", "30m",
		"--blaxel-idle-ttl", "5m",
		"--blaxel-workdir", "/workspace/app",
		"--blaxel-exec-timeout-secs", "120",
		"--blaxel-forget-missing",
	}); err != nil {
		t.Fatal(err)
	}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Blaxel.APIURL != "https://API.BLAXEL.AI:443/v1/" ||
		cfg.Blaxel.Workspace != "workspace-test" ||
		cfg.Blaxel.Region != "us-pdx-1" ||
		cfg.Blaxel.Image != "ubuntu:24.04" ||
		cfg.Blaxel.MemoryMB != 2048 ||
		cfg.Blaxel.TTL != "30m" ||
		cfg.Blaxel.IdleTTL != "5m" ||
		cfg.Blaxel.Workdir != "/workspace/app" ||
		cfg.Blaxel.ExecTimeoutSecs != 120 ||
		!cfg.Blaxel.ForgetMissing {
		t.Fatalf("cfg.Blaxel=%#v", cfg.Blaxel)
	}
}

func TestBlaxelFlagPresenceAndValidationOrder(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = "blaxel"
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterBlaxelProviderFlags(fs, cfg)
	fs.VisitAll(func(f *flag.Flag) {
		if strings.Contains(f.Name, "key") {
			t.Fatal("API key flag introduced")
		}
	})
	cfg.Blaxel = core.BlaxelConfig{APIURL: "https://example.invalid/api", Workspace: "workspace", Region: "region", Image: "image", MemoryMB: 20, TTL: "30m", IdleTTL: "5m", Workdir: "/workspace/app", ExecTimeoutSecs: 30, ForgetMissing: true}
	before := cfg.Blaxel
	if err := ApplyBlaxelProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Blaxel != before {
		t.Fatal("unvisited flags changed config")
	}
	if err := fs.Parse([]string{"--blaxel-api-url=", "--blaxel-workspace=", "--blaxel-region=", "--blaxel-image=", "--blaxel-memory-mb=0", "--blaxel-ttl=", "--blaxel-idle-ttl=", "--blaxel-workdir=", "--blaxel-exec-timeout-secs=0", "--blaxel-forget-missing=false"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyBlaxelProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Blaxel != (core.BlaxelConfig{}) {
		t.Fatal("explicit zeros not copied")
	}
	for _, name := range []string{"blaxel", " Blaxel "} {
		cfg := Config{Provider: name}
		fs := flag.NewFlagSet("guard", flag.ContinueOnError)
		fs.String("class", "", "")
		fs.String("type", "", "")
		values := RegisterBlaxelProviderFlags(fs, cfg)
		for _, args := range [][]string{{"--type=vm"}, {"--type=vm", "--class=large"}} {
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			want := "--type is not supported for provider=blaxel; use --blaxel-image"
			if len(args) == 2 {
				want = "--class is not supported for provider=blaxel; use --blaxel-memory-mb"
			}
			for _, v := range []any{nil, struct{}{}, values} {
				err := ApplyBlaxelProviderFlags(&cfg, fs, v)
				if err == nil || err.Error() != want {
					t.Fatalf("guard=%v", err)
				}
			}
		}
	}
	cfg = Config{Blaxel: BlaxelConfig{APIURL: " ", MemoryMB: -1, ExecTimeoutSecs: -1, Workdir: "relative"}}
	if err := ApplyBlaxelProviderFlags(&cfg, flag.NewFlagSet("foreign", flag.ContinueOnError), struct{}{}); err != nil {
		t.Fatal("foreign values reached validation")
	}
	if err := validateBlaxelConfig(cfg); err == nil || err.Error() != "provider=blaxel API URL must be an absolute HTTP(S) URL" {
		t.Fatalf("endpoint order=%v", err)
	}
	cfg.Blaxel.APIURL = "https://example.invalid/api"
	if err := validateBlaxelConfig(cfg); err == nil || err.Error() != "blaxel memory-mb must be >= 0" {
		t.Fatalf("memory order=%v", err)
	}
	cfg.Blaxel.MemoryMB = 0
	if err := validateBlaxelConfig(cfg); err == nil || err.Error() != "blaxel execTimeoutSecs must be non-negative" {
		t.Fatalf("timeout order=%v", err)
	}
	cfg.Blaxel.ExecTimeoutSecs = 0
	if err := validateBlaxelConfig(cfg); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("workdir order=%v", err)
	}
}

func TestValidateBlaxelConfigRejectsUnsafeValues(t *testing.T) {
	tests := []core.BlaxelConfig{
		{APIURL: "https://token@example.test"},
		{APIURL: "https://api.example.test?token=abc"},
		{APIURL: "https://api.example.test/#frag"},
		{APIURL: "http://api.example.test"},
		{APIURL: "https://api.blaxel.ai", MemoryMB: -1},
		{APIURL: "https://api.blaxel.ai", ExecTimeoutSecs: -1},
		{APIURL: "https://api.blaxel.ai", Workdir: "relative"},
	}
	for _, tc := range tests {
		err := validateBlaxelConfig(core.Config{Blaxel: tc})
		if err == nil {
			t.Fatalf("validateBlaxelConfig(%#v) succeeded", tc)
		}
		if strings.Contains(err.Error(), "token=abc") {
			t.Fatalf("error leaked URL query secret: %v", err)
		}
	}
}
