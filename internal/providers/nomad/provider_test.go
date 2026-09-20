package nomad

import (
	"flag"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecIsDelegatedRunLinuxWithoutAliases(t *testing.T) {
	p := Provider{}
	if p.Spec().Name != providerName {
		t.Fatalf("Name=%q", p.Spec().Name)
	}
	if aliases := p.Spec().Aliases; len(aliases) != 0 {
		t.Fatalf("Aliases=%v, want none", aliases)
	}
	spec := p.Spec()
	if spec.Name != providerName || spec.Family != providerName {
		t.Fatalf("spec identity=%#v", spec)
	}
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("spec.Kind=%q", spec.Kind)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec.Coordinator=%q", spec.Coordinator)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("spec.Targets=%#v", spec.Targets)
	}
	if !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("spec.Features=%#v, want cleanup after Wave 2 lifecycle", spec.Features)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) {
		t.Fatalf("spec.Features=%#v, want archive-sync after Wave 3 sync implementation", spec.Features)
	}
	if !spec.Features.Has(core.FeatureRunSession) {
		t.Fatalf("spec.Features=%#v, want reusable run sessions", spec.Features)
	}
}

func TestFlagsApplyWithoutTokenArgv(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterNomadProviderFlags(fs, cfg)
	if fs.Lookup("nomad-token") != nil {
		t.Fatal("nomad-token argv flag must not exist")
	}
	if got := fs.Lookup("nomad-datacenters").Usage; got != "comma-separated Nomad datacenters for later delegated runs" {
		t.Fatalf("datacenters help=%q", got)
	}
	args := []string{
		"--nomad-address", "https://nomad.example.test:4646",
		"--nomad-region", "global",
		"--nomad-namespace", "team-a",
		"--nomad-token-env", "TEAM_A_NOMAD_TOKEN",
		"--nomad-ca-cert", "/certs/ca.pem",
		"--nomad-ca-path", "/certs",
		"--nomad-client-cert", "/certs/client.pem",
		"--nomad-client-key", "/certs/client.key",
		"--nomad-tls-server-name", "nomad.example.test",
		"--nomad-skip-verify",
		"--nomad-task", "test-task",
		"--nomad-driver", "raw_exec",
		"--nomad-image", "busybox:latest",
		"--nomad-workdir", "/workspace/test",
		"--nomad-jobspec-template", "/templates/job.hcl",
		"--nomad-node-pool", "pool-a",
		"--nomad-datacenters", "dc1, dc2",
		"--nomad-cpu", "500",
		"--nomad-memory-mb", "1024",
		"--nomad-disk-mb", "2048",
		"--nomad-alloc-ready-timeout", "2m",
		"--nomad-eval-timeout", "3m",
		"--nomad-exec-timeout-secs", "45",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := ApplyNomadProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Nomad.Address != "https://nomad.example.test:4646" ||
		cfg.Nomad.Region != "global" ||
		cfg.Nomad.Namespace != "team-a" ||
		cfg.Nomad.TokenEnv != "TEAM_A_NOMAD_TOKEN" ||
		cfg.Nomad.CACert != "/certs/ca.pem" ||
		cfg.Nomad.CAPath != "/certs" ||
		cfg.Nomad.ClientCert != "/certs/client.pem" ||
		cfg.Nomad.ClientKey != "/certs/client.key" ||
		cfg.Nomad.TLSServerName != "nomad.example.test" ||
		!cfg.Nomad.SkipVerify ||
		cfg.Nomad.Task != "test-task" ||
		cfg.Nomad.Driver != "raw_exec" ||
		cfg.Nomad.Image != "busybox:latest" ||
		cfg.Nomad.Workdir != "/workspace/test" ||
		cfg.Nomad.JobSpecTemplate != "/templates/job.hcl" ||
		cfg.Nomad.NodePool != "pool-a" ||
		strings.Join(cfg.Nomad.Datacenters, ",") != "dc1,dc2" ||
		cfg.Nomad.CPU != 500 ||
		cfg.Nomad.MemoryMB != 1024 ||
		cfg.Nomad.DiskMB != 2048 ||
		cfg.Nomad.AllocReadyTimeout.String() != "2m0s" ||
		cfg.Nomad.EvalTimeout.String() != "3m0s" ||
		cfg.Nomad.ExecTimeoutSecs != 45 {
		t.Fatalf("cfg.Nomad=%#v", cfg.Nomad)
	}
}

func TestValidateConfigRejectsUnsafeValues(t *testing.T) {
	base := core.BaseConfig()
	base.Provider = providerName
	base.TargetOS = core.TargetLinux
	base.Nomad.Address = "https://nomad.example.test:4646"
	for _, tc := range []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{name: "non linux target", mut: func(c *Config) { c.TargetOS = core.TargetMacOS }, want: "target must be linux"},
		{name: "address credentials", mut: func(c *Config) { c.Nomad.Address = "https://user:pass@nomad.example.test" }, want: "must not include credentials"},
		{name: "address query", mut: func(c *Config) { c.Nomad.Address = "https://nomad.example.test?token=secret" }, want: "must not include credentials"},
		{name: "address fragment", mut: func(c *Config) { c.Nomad.Address = "https://nomad.example.test#secret" }, want: "must not include credentials"},
		{name: "bad scheme", mut: func(c *Config) { c.Nomad.Address = "ftp://nomad.example.test" }, want: "scheme"},
		{name: "bad token env", mut: func(c *Config) { c.Nomad.TokenEnv = "BAD-NAME" }, want: "tokenEnv"},
		{name: "relative workdir", mut: func(c *Config) { c.Nomad.Workdir = "workspace" }, want: "absolute"},
		{name: "root workdir", mut: func(c *Config) { c.Nomad.Workdir = "/" }, want: "too broad"},
		{name: "workspace root workdir", mut: func(c *Config) { c.Nomad.Workdir = "/workspace" }, want: "too broad"},
		{name: "system workdir", mut: func(c *Config) { c.Nomad.Workdir = "/etc" }, want: "too broad"},
		{name: "docker without image", mut: func(c *Config) { c.Nomad.Driver = "docker"; c.Nomad.Image = "" }, want: "image"},
		{name: "negative cpu", mut: func(c *Config) { c.Nomad.CPU = -1 }, want: "cpu"},
		{name: "negative memory", mut: func(c *Config) { c.Nomad.MemoryMB = -1 }, want: "memoryMB"},
		{name: "negative disk", mut: func(c *Config) { c.Nomad.DiskMB = -1 }, want: "diskMB"},
		{name: "negative exec", mut: func(c *Config) { c.Nomad.ExecTimeoutSecs = -1 }, want: "execTimeoutSecs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mut(&cfg)
			err := validateConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateConfigAllowsNonImageDriversAndTemplatesWithoutImage(t *testing.T) {
	base := core.BaseConfig()
	base.Provider = providerName
	base.TargetOS = core.TargetLinux
	base.Nomad.Address = "https://nomad.example.test:4646"

	for _, tc := range []struct {
		name   string
		driver string
		tpl    string
	}{
		{name: "raw exec", driver: "raw_exec"},
		{name: "docker template", driver: "docker", tpl: "/templates/job.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.Nomad.Driver = tc.driver
			cfg.Nomad.Image = ""
			cfg.Nomad.JobSpecTemplate = tc.tpl
			if err := validateConfig(cfg); err != nil {
				t.Fatalf("validateConfig() error=%v", err)
			}
		})
	}
}

func TestNomadBindingFlagPartialApplication(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterNomadProviderFlags(fs, cfg)
	if err := fs.Parse([]string{"--nomad-task=", "--nomad-datacenters= first ,,first ", "--nomad-alloc-ready-timeout=invalid", "--nomad-eval-timeout=2m", "--nomad-exec-timeout-secs=12"}); err != nil {
		t.Fatal(err)
	}
	want := cfg
	want.Nomad.Task = ""
	want.Nomad.Datacenters = []string{"first", "first"}
	core.RecordProviderFlagInputs(&want, true, providerName)
	err := ApplyNomadProviderFlags(&cfg, fs, values)
	if err == nil || err.Error() != "nomad alloc ready timeout must be a positive duration" {
		t.Fatalf("flag error=%v", err)
	}
	if !reflect.DeepEqual(cfg, want) || cfg.Nomad.EvalTimeout != 5*time.Minute {
		t.Fatal("flag error changed partial assignment or provenance boundary")
	}
}

func TestNomadBindingFlagsPreservePresenceAndLastList(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Nomad.Address = "https://nomad.example.test"
	cfg.Nomad.Datacenters = []string{"prior"}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterNomadProviderFlags(fs, cfg)
	before := cfg
	if err := ApplyNomadProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, before) {
		t.Fatal("unvisited flags changed configuration or provenance")
	}
	if err := fs.Parse([]string{"--nomad-datacenters=first", "--nomad-datacenters= last ,,last "}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyNomadProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Nomad.Datacenters, []string{"last", "last"}) {
		t.Fatalf("last scalar list flag=%v", cfg.Nomad.Datacenters)
	}
}

func TestNomadTokenVariableNameFallback(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{{"", "NOMAD_TOKEN"}, {"  ", "NOMAD_TOKEN"}, {" NOMAD_TOKEN ", "NOMAD_TOKEN"}, {" CUSTOM_NAME ", "CUSTOM_NAME"}} {
		cfg := core.Config{Nomad: core.NomadConfig{TokenEnv: tc.raw}}
		if got := nomadTokenEnv(cfg); got != tc.want {
			t.Fatalf("variable name=%q want=%q", got, tc.want)
		}
	}
}
