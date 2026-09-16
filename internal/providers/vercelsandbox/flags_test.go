package vercelsandbox

import (
	"flag"
	"io"
	"math"
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
	if !spec.Features.Has(core.FeatureArchiveSync) || !spec.Features.Has(core.FeatureCleanup) || !spec.Features.Has(core.FeatureRunSession) {
		t.Fatalf("features=%v", spec.Features)
	}
	if aliases := (Provider{}).Spec().Aliases; len(aliases) != 0 {
		t.Fatalf("aliases=%v, want none", aliases)
	}
}

func TestVercelSandboxFlagsApplyAndValidate(t *testing.T) {
	cfg := core.Config{Provider: providerName}
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
		"--vercel-sandbox-network-deny", "169.254.169.254/32",
		"--vercel-sandbox-ports", "3000,8080-8082",
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
		cfg := core.Config{Provider: providerName}
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
	valid := core.Config{}
	valid.VercelSandbox.Runtime = defaultRuntime
	valid.VercelSandbox.Workdir = defaultWorkdir

	tests := []struct {
		name string
		mut  func(*core.Config)
		want string
	}{
		{"runtime", func(c *core.Config) { c.VercelSandbox.Runtime = "ruby" }, "runtime"},
		{"removed-runtime", func(c *core.Config) { c.VercelSandbox.Runtime = "node20" }, "runtime"},
		{"workdir-relative", func(c *core.Config) { c.VercelSandbox.Workdir = "workspace" }, "absolute"},
		{"workdir-broad", func(c *core.Config) { c.VercelSandbox.Workdir = "/vercel/sandbox" }, "too broad"},
		{"project-without-team", func(c *core.Config) { c.VercelSandbox.ProjectID = "prj_123" }, "requires teamId or scope"},
		{"timeout", func(c *core.Config) { c.VercelSandbox.TimeoutSecs = -1 }, "non-negative"},
		{"exec-timeout", func(c *core.Config) { c.VercelSandbox.ExecTimeoutSecs = -1 }, "non-negative"},
		{"vcpus", func(c *core.Config) { c.VercelSandbox.VCPUs = -1 }, "vcpus"},
		{"vcpus too small", func(c *core.Config) { c.VercelSandbox.VCPUs = 0.1 }, "at least 0.25"},
		{"vcpus NaN", func(c *core.Config) { c.VercelSandbox.VCPUs = math.NaN() }, "must be finite"},
		{"vcpus positive infinity", func(c *core.Config) { c.VercelSandbox.VCPUs = math.Inf(1) }, "must be finite"},
		{"vcpus negative infinity", func(c *core.Config) { c.VercelSandbox.VCPUs = math.Inf(-1) }, "must be finite"},
		{"network-policy", func(c *core.Config) { c.VercelSandbox.NetworkPolicy = "mystery" }, "networkPolicy"},
		{"network-none-with-rules", func(c *core.Config) {
			c.VercelSandbox.NetworkPolicy = "none"
			c.VercelSandbox.NetworkAllow = []string{"api.example.com"}
		}, "cannot be combined"},
		{"network-entry", func(c *core.Config) { c.VercelSandbox.NetworkAllow = []string{"bad_host!"} }, "networkAllow"},
		{"network-domain-deny", func(c *core.Config) { c.VercelSandbox.NetworkDeny = []string{"blocked.example.com"} }, "does not support domain deny"},
		{"port", func(c *core.Config) { c.VercelSandbox.Ports = []string{"70000"} }, "port"},
		{"too-many-ports", func(c *core.Config) { c.VercelSandbox.Ports = []string{"3000-3015"} }, "at most 15"},
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

func TestVercelSandboxVCPUFlagValidation(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  float64
		err   string
	}{
		{"NaN", 0, "vercel-sandbox vcpus must be finite"},
		{"+Inf", 0, "vercel-sandbox vcpus must be finite"},
		{"-Inf", 0, "vercel-sandbox vcpus must be finite"},
		{"-1", 0, "vercel-sandbox vcpus must be positive when set"},
		{"0.1", 0, "vercel-sandbox vcpus must be at least 0.25 when set"},
		{"0", 0, ""},
		{"0.25", 0.25, ""},
		{"1.5", 1.5, ""},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			values := RegisterVercelSandboxProviderFlags(fs, cfg)
			if err := fs.Parse([]string{"--vercel-sandbox-vcpus=" + tc.value}); err != nil {
				t.Fatalf("float parser rejected value before provider validation: %v", err)
			}
			err := ApplyVercelSandboxProviderFlags(&cfg, fs, values)
			if tc.err != "" {
				var exitErr core.ExitError
				if !core.AsExitError(err, &exitErr) || exitErr.Code != 2 || err.Error() != tc.err {
					t.Fatalf("validation error=%v, want exit 2 with %q", err, tc.err)
				}
				return
			}
			if err != nil || cfg.VercelSandbox.VCPUs != tc.want {
				t.Fatalf("vcpus=%v error=%v, want %v unchanged", cfg.VercelSandbox.VCPUs, err, tc.want)
			}
		})
	}
}

func TestValidateVercelSandboxConfigAcceptsProjectWithTeamOrScope(t *testing.T) {
	for _, cfg := range []core.Config{
		func() core.Config {
			cfg := core.Config{}
			cfg.VercelSandbox.Runtime = defaultRuntime
			cfg.VercelSandbox.Workdir = defaultWorkdir
			cfg.VercelSandbox.ProjectID = "prj_123"
			cfg.VercelSandbox.TeamID = "team_123"
			return cfg
		}(),
		func() core.Config {
			cfg := core.Config{}
			cfg.VercelSandbox.Runtime = defaultRuntime
			cfg.VercelSandbox.Workdir = defaultWorkdir
			cfg.VercelSandbox.ProjectID = "prj_123"
			cfg.VercelSandbox.Scope = "example-org"
			return cfg
		}(),
	} {
		if err := validateVercelSandboxConfig(cfg); err != nil {
			t.Fatalf("validate project scope %#v: %v", cfg.VercelSandbox, err)
		}
	}
}

func TestValidateVercelSandboxConfigAcceptsCurrentRuntimes(t *testing.T) {
	for _, runtime := range []string{"node26", "node24", "node22", "python3.13"} {
		t.Run(runtime, func(t *testing.T) {
			cfg := core.Config{}
			cfg.VercelSandbox.Runtime = runtime
			cfg.VercelSandbox.Workdir = defaultWorkdir
			if err := validateVercelSandboxConfig(cfg); err != nil {
				t.Fatalf("validate runtime %q: %v", runtime, err)
			}
		})
	}
}

func TestVercelSandboxFlagPresence(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{true: "explicit zero values", false: "omitted"}[explicit], func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			cfg.VercelSandbox.Persistent = true
			cfg.VercelSandbox.VCPUs = 2
			cfg.VercelSandbox.Ports = []string{"3000"}
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			values := RegisterVercelSandboxProviderFlags(fs, core.BaseConfig())
			var args []string
			if explicit {
				args = []string{"--vercel-sandbox-runtime=", "--vercel-sandbox-vcpus=0", "--vercel-sandbox-persistent=false", "--vercel-sandbox-ports="}
			}
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			if err := ApplyVercelSandboxProviderFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if explicit {
				if cfg.VercelSandbox.Runtime != "" || cfg.VercelSandbox.VCPUs != 0 || cfg.VercelSandbox.Persistent || len(cfg.VercelSandbox.Ports) != 0 {
					t.Fatalf("explicit zero values not applied: %#v", cfg.VercelSandbox)
				}
			} else if cfg.VercelSandbox.VCPUs != 2 || !cfg.VercelSandbox.Persistent || len(cfg.VercelSandbox.Ports) != 1 {
				t.Fatalf("registration defaults overrode config: %#v", cfg.VercelSandbox)
			}
		})
	}
}

func TestVercelSandboxFlagParsingAndNoSecretFlags(t *testing.T) {
	for _, arg := range []string{"--vercel-sandbox-vcpus=nope", "--vercel-sandbox-timeout-secs=nope", "--vercel-sandbox-persistent=nope", "--vercel-sandbox-token=fake", "--vercel-sandbox-auth-token=fake", "--vercel-sandbox-api-url=https://untrusted.example"} {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		RegisterVercelSandboxProviderFlags(fs, core.BaseConfig())
		if err := fs.Parse([]string{arg}); err == nil {
			t.Fatalf("accepted %s", arg)
		}
	}
}
