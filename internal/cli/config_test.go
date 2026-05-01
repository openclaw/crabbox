package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigFromUserFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "")
	path := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`broker:
  url: https://crabbox.example.test
  token: secret
  provider: aws
class: standard
lease:
  ttl: 2h
  idleTimeout: 45m
aws:
  region: eu-west-1
  rootGB: 800
  sshCIDRs:
    - 198.51.100.7/32
sync:
  checksum: true
  gitSeed: false
  baseRef: trunk
  timeout: 30m
  warnFiles: 100
  warnBytes: 200
  failFiles: 300
  failBytes: 400
  allowLarge: true
  exclude:
    - .artifacts
    - tmp
env:
  allow:
    - CI
    - NODE_OPTIONS
    - CUSTOM_*
capacity:
  market: spot
  strategy: most-available
  fallback: on-demand-after-120s
  regions:
    - eu-west-1
actions:
  repo: openclaw/crabbox
  workflow: .github/workflows/crabbox.yml
  job: hydrate
  ref: main
  fields:
    - crabbox_docker_cache=true
    - crabbox_prepare_images=1
  runnerLabels:
    - crabbox
    - linux-large
  runnerVersion: latest
  ephemeral: false
blacksmith:
  org: openclaw
  workflow: .github/workflows/blacksmith-testbox.yml
  job: hydrate
  ref: main
  idleTimeout: 90m
  debug: true
results:
  junit:
    - junit.xml
cache:
  pnpm: true
  npm: false
  docker: true
  git: true
  maxGB: 120
  purgeOnRelease: true
ssh:
  key: ~/.ssh/crabbox
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "aws" {
		t.Fatalf("Provider=%q want aws", cfg.Provider)
	}
	if cfg.ServerType != "c7a.8xlarge" {
		t.Fatalf("ServerType=%q want c7a.8xlarge", cfg.ServerType)
	}
	if cfg.Coordinator != "https://crabbox.example.test" || cfg.CoordToken != "secret" {
		t.Fatalf("broker config not loaded: %#v", cfg)
	}
	if cfg.TTL.String() != "2h0m0s" || cfg.IdleTimeout.String() != "45m0s" {
		t.Fatalf("lease config not loaded: ttl=%s idle=%s", cfg.TTL, cfg.IdleTimeout)
	}
	if cfg.AWSRootGB != 800 {
		t.Fatalf("AWSRootGB=%d want 800", cfg.AWSRootGB)
	}
	if len(cfg.AWSSSHCIDRs) != 1 || cfg.AWSSSHCIDRs[0] != "198.51.100.7/32" {
		t.Fatalf("AWSSSHCIDRs=%v", cfg.AWSSSHCIDRs)
	}
	if cfg.SSHKey != filepath.Join(home, ".ssh", "crabbox") {
		t.Fatalf("SSHKey=%q", cfg.SSHKey)
	}
	if !cfg.Sync.Checksum || cfg.Sync.GitSeed || cfg.Sync.BaseRef != "trunk" {
		t.Fatalf("sync config not loaded: %#v", cfg.Sync)
	}
	if cfg.Sync.Timeout.String() != "30m0s" || cfg.Sync.WarnFiles != 100 || cfg.Sync.WarnBytes != 200 || cfg.Sync.FailFiles != 300 || cfg.Sync.FailBytes != 400 || !cfg.Sync.AllowLarge {
		t.Fatalf("sync guardrails not loaded: %#v", cfg.Sync)
	}
	if len(cfg.Sync.Excludes) != 2 || cfg.Sync.Excludes[0] != ".artifacts" || cfg.Sync.Excludes[1] != "tmp" {
		t.Fatalf("sync excludes not loaded: %#v", cfg.Sync.Excludes)
	}
	if len(cfg.EnvAllow) != 3 || cfg.EnvAllow[2] != "CUSTOM_*" {
		t.Fatalf("env allow not loaded: %#v", cfg.EnvAllow)
	}
	if cfg.Capacity.Strategy != "most-available" || len(cfg.Capacity.Regions) != 1 || cfg.Capacity.Regions[0] != "eu-west-1" {
		t.Fatalf("capacity config not loaded: %#v", cfg.Capacity)
	}
	if cfg.Actions.Repo != "openclaw/crabbox" || cfg.Actions.Workflow != ".github/workflows/crabbox.yml" || cfg.Actions.Job != "hydrate" || cfg.Actions.Ref != "main" {
		t.Fatalf("actions config not loaded: %#v", cfg.Actions)
	}
	if len(cfg.Actions.Fields) != 2 || cfg.Actions.Fields[0] != "crabbox_docker_cache=true" || cfg.Actions.Fields[1] != "crabbox_prepare_images=1" {
		t.Fatalf("actions fields config not loaded: %#v", cfg.Actions.Fields)
	}
	if cfg.Actions.Ephemeral || len(cfg.Actions.RunnerLabels) != 2 || cfg.Actions.RunnerLabels[1] != "linux-large" {
		t.Fatalf("actions runner config not loaded: %#v", cfg.Actions)
	}
	if cfg.Blacksmith.Org != "openclaw" || cfg.Blacksmith.Workflow != ".github/workflows/blacksmith-testbox.yml" || cfg.Blacksmith.Job != "hydrate" || cfg.Blacksmith.Ref != "main" || cfg.Blacksmith.IdleTimeout != 90*time.Minute || !cfg.Blacksmith.Debug {
		t.Fatalf("blacksmith config not loaded: %#v", cfg.Blacksmith)
	}
	if len(cfg.Results.JUnit) != 1 || cfg.Results.JUnit[0] != "junit.xml" {
		t.Fatalf("results config not loaded: %#v", cfg.Results)
	}
	if !cfg.Cache.Pnpm || cfg.Cache.Npm || !cfg.Cache.Docker || !cfg.Cache.Git || cfg.Cache.MaxGB != 120 || !cfg.Cache.PurgeOnRelease {
		t.Fatalf("cache config not loaded: %#v", cfg.Cache)
	}
}

func TestEnvOverridesConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "hetzner")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "fast")
	t.Setenv("CRABBOX_TTL", "3h")
	t.Setenv("CRABBOX_IDLE_TIMEOUT", "20m")
	t.Setenv("CRABBOX_AWS_SSH_CIDRS", "198.51.100.7/32,203.0.113.8/32")
	path := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("provider: aws\nclass: beast\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "hetzner" || cfg.Class != "fast" || cfg.ServerType != "ccx43" || cfg.TTL.String() != "3h0m0s" || cfg.IdleTimeout.String() != "20m0s" {
		t.Fatalf("unexpected config: provider=%s class=%s type=%s ttl=%s idle=%s", cfg.Provider, cfg.Class, cfg.ServerType, cfg.TTL, cfg.IdleTimeout)
	}
	if len(cfg.AWSSSHCIDRs) != 2 || cfg.AWSSSHCIDRs[0] != "198.51.100.7/32" || cfg.AWSSSHCIDRs[1] != "203.0.113.8/32" {
		t.Fatalf("AWSSSHCIDRs=%v", cfg.AWSSSHCIDRs)
	}
}

func TestRepoConfigIsYamlOnly(t *testing.T) {
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "")
	if err := os.WriteFile(".crabbox.json", []byte(`{"profile":"json-profile","provider":"aws"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".crabbox.yaml", []byte("profile: yaml-profile\nprovider: aws\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != "yaml-profile" || cfg.Provider != "aws" {
		t.Fatalf("unexpected config: profile=%s provider=%s", cfg.Profile, cfg.Provider)
	}
}

func TestConfigHelperBranches(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "explicit.yaml"))

	if got := configPaths(); len(got) != 1 || got[0] != os.Getenv("CRABBOX_CONFIG") {
		t.Fatalf("configPaths=%v", got)
	}
	if got := writableConfigPath(); got != os.Getenv("CRABBOX_CONFIG") {
		t.Fatalf("writableConfigPath=%q", got)
	}

	cfgPath, err := writeUserFileConfig(fileConfig{Profile: "written", Provider: "aws"})
	if err != nil {
		t.Fatal(err)
	}
	if cfgPath != os.Getenv("CRABBOX_CONFIG") {
		t.Fatalf("write path=%q", cfgPath)
	}
	file, err := readFileConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if file.Profile != "written" || file.Provider != "aws" {
		t.Fatalf("file config=%#v", file)
	}

	empty, err := readFileConfig(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if empty.Profile != "" {
		t.Fatalf("missing file config=%#v", empty)
	}

	if got := expandUserPath("~"); got != home {
		t.Fatalf("expand ~= %q want %q", got, home)
	}
	if got := expandUserPath("~/bin"); got != filepath.Join(home, "bin") {
		t.Fatalf("expand ~/bin=%q", got)
	}
	if got := expandUserPath("/tmp/x"); got != "/tmp/x" {
		t.Fatalf("absolute path changed to %q", got)
	}

	duration := 10 * time.Minute
	applyLeaseDuration(&duration, "")
	applyLeaseDuration(&duration, "bad")
	applyLeaseDuration(&duration, "0s")
	if duration != 10*time.Minute {
		t.Fatalf("invalid durations changed value to %s", duration)
	}
	applyLeaseDuration(&duration, "15m")
	if duration != 15*time.Minute {
		t.Fatalf("duration=%s", duration)
	}
}

func TestEnvHelperBranches(t *testing.T) {
	t.Setenv("CRABBOX_INT", "42")
	t.Setenv("CRABBOX_BAD_INT", "oops")
	if got := getenvInt("CRABBOX_INT", 7); got != 42 {
		t.Fatalf("int=%d", got)
	}
	if got := getenvInt("CRABBOX_BAD_INT", 7); got != 7 {
		t.Fatalf("bad int fallback=%d", got)
	}
	if got := getenvInt("CRABBOX_MISSING_INT", 7); got != 7 {
		t.Fatalf("missing int fallback=%d", got)
	}

	for _, tc := range []struct {
		name  string
		value string
		want  bool
		ok    bool
	}{
		{"CRABBOX_BOOL_TRUE", "yes", true, true},
		{"CRABBOX_BOOL_FALSE", "off", false, true},
		{"CRABBOX_BOOL_BAD", "maybe", false, false},
		{"CRABBOX_BOOL_EMPTY", "", false, false},
	} {
		if tc.value != "" {
			t.Setenv(tc.name, tc.value)
		}
		got, ok := getenvBool(tc.name)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("getenvBool(%s)=%v,%v want %v,%v", tc.name, got, ok, tc.want, tc.ok)
		}
	}

	list := splitCommaList(" CI, ,NODE_OPTIONS,CUSTOM_* ")
	if len(list) != 3 || list[0] != "CI" || list[2] != "CUSTOM_*" {
		t.Fatalf("splitCommaList=%v", list)
	}
}
