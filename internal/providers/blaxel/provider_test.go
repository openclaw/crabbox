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
	if !spec.Features.Has(core.FeatureArchiveSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("Features=%#v, want archive-sync and cleanup", spec.Features)
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
	cfg := core.Config{Provider: providerName, Blaxel: core.BlaxelConfig{APIURL: defaultAPIURL, Workdir: defaultWorkdir}}
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

func TestValidateBlaxelConfigRejectsUnsafeValues(t *testing.T) {
	tests := []core.BlaxelConfig{
		{APIURL: "https://token@example.test"},
		{APIURL: "https://api.example.test?token=abc"},
		{APIURL: "https://api.example.test/#frag"},
		{APIURL: "http://api.example.test"},
		{APIURL: defaultAPIURL, MemoryMB: -1},
		{APIURL: defaultAPIURL, ExecTimeoutSecs: -1},
		{APIURL: defaultAPIURL, Workdir: "relative"},
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
