package flue

import (
	"bytes"
	"context"
	"flag"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecMatchesContract(t *testing.T) {
	p := Provider{}
	if p.Name() != providerName {
		t.Fatalf("Name=%q want %q", p.Name(), providerName)
	}
	if p.Aliases() != nil {
		t.Fatalf("Aliases=%v, want nil", p.Aliases())
	}
	spec := p.Spec()
	if spec.Name != providerName || spec.Family != providerName {
		t.Fatalf("spec identity=%#v", spec)
	}
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("Kind=%q want delegated-run", spec.Kind)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("Targets=%#v, want Linux only", spec.Targets)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) || len(spec.Features) != 1 {
		t.Fatalf("Features=%#v, want archive-sync only", spec.Features)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("Coordinator=%q want never", spec.Coordinator)
	}
}

func TestApplyFlagsAndValidate(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := Provider{}.RegisterFlags(fs, cfg)
	args := []string{
		"--flue-cli", "/opt/flue/bin/flue",
		"--flue-root", "/repo/flue",
		"--flue-workflow", "workflow:test",
		"--flue-target", "node",
		"--flue-config", "/repo/flue/flue.config.ts",
		"--flue-env", "/repo/flue/.env",
		"--flue-output", "json",
		"--flue-workdir", "/workspace/app",
		"--flue-timeout-secs", "123",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	want := core.FlueConfig{
		CLIPath:     "/opt/flue/bin/flue",
		Root:        "/repo/flue",
		Workflow:    "workflow:test",
		Target:      "node",
		Config:      "/repo/flue/flue.config.ts",
		EnvFile:     "/repo/flue/.env",
		Output:      "json",
		Workdir:     "/workspace/app",
		TimeoutSecs: 123,
	}
	if cfg.Flue != want {
		t.Fatalf("cfg.Flue=%#v want %#v", cfg.Flue, want)
	}
}

func TestApplyFlagsRejectsUnsupportedGenericSizing(t *testing.T) {
	for _, flagName := range []string{"class", "type"} {
		cfg := core.BaseConfig()
		cfg.Provider = providerName
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		values := Provider{}.RegisterFlags(fs, cfg)
		fs.String(flagName, "", "generic")
		if err := fs.Parse([]string{"--" + flagName, "x"}); err != nil {
			t.Fatal(err)
		}
		err := Provider{}.ApplyFlags(&cfg, fs, values)
		if err == nil || !strings.Contains(err.Error(), "--"+flagName+" is not supported") {
			t.Fatalf("ApplyFlags with --%s err=%v", flagName, err)
		}
	}
}

func TestApplyFlagsAllowsUnsupportedTargetForDoctorDiagnostics(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := Provider{}.RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{"--flue-target", "cloudflare"}); err != nil {
		t.Fatal(err)
	}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatalf("ApplyFlags err=%v", err)
	}
	if cfg.Flue.Target != "cloudflare" {
		t.Fatalf("target=%q want cloudflare", cfg.Flue.Target)
	}
}

func TestValidateFlueConfigRejectsUnsupportedValues(t *testing.T) {
	tests := []struct {
		name string
		edit func(*core.Config)
		want string
	}{
		{
			name: "empty cli",
			edit: func(cfg *core.Config) { cfg.Flue.CLIPath = "" },
			want: "cliPath",
		},
		{
			name: "empty workflow",
			edit: func(cfg *core.Config) { cfg.Flue.Workflow = "" },
			want: "workflow",
		},
		{
			name: "cloudflare target",
			edit: func(cfg *core.Config) { cfg.Flue.Target = "cloudflare" },
			want: "target=node only",
		},
		{
			name: "server target",
			edit: func(cfg *core.Config) { cfg.Flue.Target = "server" },
			want: "target=node only",
		},
		{
			name: "relative workdir",
			edit: func(cfg *core.Config) { cfg.Flue.Workdir = "workspace" },
			want: "must be absolute",
		},
		{
			name: "broad workdir",
			edit: func(cfg *core.Config) { cfg.Flue.Workdir = "/workspace" },
			want: "too broad",
		},
		{
			name: "negative timeout",
			edit: func(cfg *core.Config) { cfg.Flue.TimeoutSecs = -1 },
			want: "non-negative",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			tc.edit(&cfg)
			err := ValidateFlueConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateFlueConfig err=%v want %q", err, tc.want)
			}
		})
	}
}

func TestValidateFlueRunTargetRejectsUnsupportedValues(t *testing.T) {
	for _, target := range []string{"cloudflare", "server"} {
		cfg := core.BaseConfig()
		cfg.Provider = providerName
		cfg.Flue.Target = target
		err := ValidateFlueRunTarget(cfg)
		if err == nil || !strings.Contains(err.Error(), "target=node only") {
			t.Fatalf("ValidateFlueRunTarget(%q) err=%v", target, err)
		}
	}
}

func TestConfigureReturnsDelegatedBackend(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	var out, stderr bytes.Buffer
	backend, err := Provider{}.Configure(cfg, core.Runtime{Stdout: &out, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	delegated, ok := backend.(core.DelegatedRunBackend)
	if !ok {
		t.Fatalf("backend does not implement DelegatedRunBackend: %T", backend)
	}
	_, err = delegated.Run(context.Background(), core.RunRequest{})
	if err == nil || !strings.Contains(err.Error(), "missing command") {
		t.Fatalf("Run err=%v", err)
	}
}

func TestConfigureRejectsUnsupportedTarget(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Flue.Target = "cloudflare"
	_, err := Provider{}.Configure(cfg, core.Runtime{})
	if err == nil || !strings.Contains(err.Error(), "target=node only") {
		t.Fatalf("Configure err=%v", err)
	}
}
