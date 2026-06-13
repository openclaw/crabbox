package vercelsandbox

import (
	"flag"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestVercelSandboxProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName || spec.Family != providerFamily || spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("features=%v", spec.Features)
	}
	if aliases := (Provider{}).Aliases(); len(aliases) != 0 {
		t.Fatalf("aliases=%v, want none", aliases)
	}
}

func TestVercelSandboxFlagsApplyAndValidate(t *testing.T) {
	cfg := Config{Provider: providerName}
	cfg.VercelSandbox.Runtime = defaultRuntime
	cfg.VercelSandbox.Workdir = defaultWorkdir
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("class", "", "")
	fs.String("type", "", "")
	values := RegisterVercelSandboxProviderFlags(fs, cfg)
	args := []string{
		"--vercel-sandbox-runtime", "node22",
		"--vercel-sandbox-workdir", "/work/app",
		"--vercel-sandbox-project-id", "prj_123",
		"--vercel-sandbox-team-id", "team_123",
		"--vercel-sandbox-scope", "example-org",
		"--vercel-sandbox-vcpus", "2",
		"--vercel-sandbox-timeout-secs", "120",
		"--vercel-sandbox-exec-timeout-secs", "60",
		"--vercel-sandbox-persistent",
		"--vercel-sandbox-snapshot", "snap_123",
		"--vercel-sandbox-snapshot-mode", "restore",
		"--vercel-sandbox-network-policy", "restricted",
		"--vercel-sandbox-network-allow", "api.example.com,10.0.0.0/8",
		"--vercel-sandbox-network-deny", "metadata.google.internal",
		"--vercel-sandbox-ports", "3000,8080-8090",
		"--vercel-sandbox-forget-missing",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := ApplyVercelSandboxProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.VercelSandbox.Runtime != "node22" || cfg.VercelSandbox.Workdir != "/work/app" || cfg.VercelSandbox.ProjectID != "prj_123" || cfg.VercelSandbox.TeamID != "team_123" || cfg.VercelSandbox.Scope != "example-org" {
		t.Fatalf("scalars not applied: %#v", cfg.VercelSandbox)
	}
	if cfg.VercelSandbox.VCPUs != 2 || cfg.VercelSandbox.TimeoutSecs != 120 || cfg.VercelSandbox.ExecTimeoutSecs != 60 || !cfg.VercelSandbox.Persistent || cfg.VercelSandbox.Snapshot != "snap_123" || cfg.VercelSandbox.SnapshotMode != "restore" || cfg.VercelSandbox.NetworkPolicy != "restricted" || !cfg.VercelSandbox.ForgetMissing {
		t.Fatalf("settings not applied: %#v", cfg.VercelSandbox)
	}
}

func TestVercelSandboxRejectsClassAndType(t *testing.T) {
	for _, flagName := range []string{"class", "type"} {
		cfg := Config{Provider: providerName}
		cfg.VercelSandbox.Runtime = defaultRuntime
		cfg.VercelSandbox.Workdir = defaultWorkdir
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.String("class", "", "")
		fs.String("type", "", "")
		values := RegisterVercelSandboxProviderFlags(fs, cfg)
		if err := fs.Parse([]string{"--" + flagName, "large"}); err != nil {
			t.Fatal(err)
		}
		err := ApplyVercelSandboxProviderFlags(&cfg, fs, values)
		if err == nil || !strings.Contains(err.Error(), "--"+flagName+" is not supported") {
			t.Fatalf("Apply flags with --%s err=%v", flagName, err)
		}
	}
}

func TestValidateVercelSandboxConfigRejectsInvalidValues(t *testing.T) {
	valid := Config{}
	valid.VercelSandbox.Runtime = defaultRuntime
	valid.VercelSandbox.Workdir = defaultWorkdir

	tests := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"runtime", func(c *Config) { c.VercelSandbox.Runtime = "ruby" }, "runtime"},
		{"workdir-relative", func(c *Config) { c.VercelSandbox.Workdir = "workspace" }, "absolute"},
		{"workdir-broad", func(c *Config) { c.VercelSandbox.Workdir = "/vercel/sandbox" }, "too broad"},
		{"timeout", func(c *Config) { c.VercelSandbox.TimeoutSecs = -1 }, "non-negative"},
		{"exec-timeout", func(c *Config) { c.VercelSandbox.ExecTimeoutSecs = -1 }, "non-negative"},
		{"vcpus", func(c *Config) { c.VercelSandbox.VCPUs = -1 }, "vcpus"},
		{"network-policy", func(c *Config) { c.VercelSandbox.NetworkPolicy = "mystery" }, "networkPolicy"},
		{"network-entry", func(c *Config) { c.VercelSandbox.NetworkAllow = []string{"bad_host!"} }, "networkAllow"},
		{"port", func(c *Config) { c.VercelSandbox.Ports = []string{"70000"} }, "port"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.mut(&cfg)
			err := validateVercelSandboxConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want contains %q", err, tc.want)
			}
		})
	}
}
