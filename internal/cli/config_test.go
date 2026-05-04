package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"CRABBOX_COORDINATOR",
		"CRABBOX_COORDINATOR_TOKEN",
		"CRABBOX_COORDINATOR_ADMIN_TOKEN",
		"CRABBOX_ADMIN_TOKEN",
		"CRABBOX_ACCESS_CLIENT_ID",
		"CRABBOX_ACCESS_CLIENT_SECRET",
		"CRABBOX_ACCESS_TOKEN",
		"CF_ACCESS_CLIENT_ID",
		"CF_ACCESS_CLIENT_SECRET",
		"CF_ACCESS_TOKEN",
	} {
		t.Setenv(key, "")
	}
}

func TestLoadConfigFromUserFile(t *testing.T) {
	clearConfigEnv(t)
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
  adminToken: admin-secret
  provider: aws
  access:
    clientId: access-client
    clientSecret: access-secret
    token: access-jwt
class: standard
target: windows
windows:
  mode: wsl2
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
static:
  id: win-dev
  name: windows-dev
  host: win-dev.local
  user: peter
  port: "22"
  workRoot: /home/peter/crabbox
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
  fallbackPorts:
    - "22"
    - "2022"
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
	if cfg.TargetOS != targetWindows || cfg.WindowsMode != windowsModeWSL2 {
		t.Fatalf("target config not loaded: target=%s windowsMode=%s", cfg.TargetOS, cfg.WindowsMode)
	}
	if cfg.ServerType != "c7a.8xlarge" {
		t.Fatalf("ServerType=%q want c7a.8xlarge", cfg.ServerType)
	}
	if cfg.Coordinator != "https://crabbox.example.test" || cfg.CoordToken != "secret" || cfg.CoordAdminToken != "admin-secret" {
		t.Fatalf("broker config not loaded: %#v", cfg)
	}
	if cfg.Access.ClientID != "access-client" || cfg.Access.ClientSecret != "access-secret" || cfg.Access.Token != "access-jwt" {
		t.Fatalf("access config not loaded: %#v", cfg.Access)
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
	if len(cfg.SSHFallbackPorts) != 2 || cfg.SSHFallbackPorts[0] != "22" || cfg.SSHFallbackPorts[1] != "2022" {
		t.Fatalf("SSHFallbackPorts=%v", cfg.SSHFallbackPorts)
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
	if cfg.Static.Host != "win-dev.local" || cfg.Static.User != "peter" || cfg.Static.Port != "22" || cfg.WorkRoot != "/home/peter/crabbox" {
		t.Fatalf("static config not loaded: static=%#v workRoot=%s", cfg.Static, cfg.WorkRoot)
	}
	if len(cfg.Results.JUnit) != 1 || cfg.Results.JUnit[0] != "junit.xml" {
		t.Fatalf("results config not loaded: %#v", cfg.Results)
	}
	if !cfg.Cache.Pnpm || cfg.Cache.Npm || !cfg.Cache.Docker || !cfg.Cache.Git || cfg.Cache.MaxGB != 120 || !cfg.Cache.PurgeOnRelease {
		t.Fatalf("cache config not loaded: %#v", cfg.Cache)
	}
}

func TestEnvOverridesConfig(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "hetzner")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "fast")
	t.Setenv("CRABBOX_TTL", "3h")
	t.Setenv("CRABBOX_IDLE_TIMEOUT", "20m")
	t.Setenv("CRABBOX_AWS_SSH_CIDRS", "198.51.100.7/32,203.0.113.8/32")
	t.Setenv("CRABBOX_SSH_FALLBACK_PORTS", "none")
	t.Setenv("CRABBOX_ACCESS_CLIENT_ID", "env-access-client")
	t.Setenv("CRABBOX_ACCESS_CLIENT_SECRET", "env-access-secret")
	t.Setenv("CRABBOX_ACCESS_TOKEN", "env-access-jwt")
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "env-admin-secret")
	t.Setenv("CRABBOX_TARGET", "macos")
	t.Setenv("CRABBOX_STATIC_HOST", "mac.local")
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
	if len(cfg.SSHFallbackPorts) != 0 {
		t.Fatalf("SSHFallbackPorts=%v want disabled fallback", cfg.SSHFallbackPorts)
	}
	if cfg.Access.ClientID != "env-access-client" || cfg.Access.ClientSecret != "env-access-secret" || cfg.Access.Token != "env-access-jwt" {
		t.Fatalf("unexpected access config: %#v", cfg.Access)
	}
	if cfg.CoordAdminToken != "env-admin-secret" {
		t.Fatalf("unexpected admin token state: %q", cfg.CoordAdminToken)
	}
	if cfg.TargetOS != targetMacOS || cfg.Static.Host != "mac.local" {
		t.Fatalf("unexpected target env: target=%s static=%#v", cfg.TargetOS, cfg.Static)
	}
}

func TestAccessAuthState(t *testing.T) {
	for name, tc := range map[string]struct {
		access AccessConfig
		want   string
	}{
		"missing": {
			want: "missing",
		},
		"incomplete": {
			access: AccessConfig{ClientID: "client"},
			want:   "incomplete",
		},
		"service token": {
			access: AccessConfig{ClientID: "client", ClientSecret: "secret"},
			want:   "service-token",
		},
		"token": {
			access: AccessConfig{Token: "jwt"},
			want:   "token",
		},
		"service token plus token": {
			access: AccessConfig{ClientID: "client", ClientSecret: "secret", Token: "jwt"},
			want:   "service-token+token",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := accessAuthState(tc.access); got != tc.want {
				t.Fatalf("accessAuthState()=%q want %q", got, tc.want)
			}
		})
	}
}

func TestRepoConfigIsYamlOnly(t *testing.T) {
	clearConfigEnv(t)
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
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode=%04o want 0600", got)
	}

	if err := os.Chmod(cfgPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeUserFileConfig(fileConfig{Profile: "rewritten"}); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("rewritten config mode=%04o want 0600", got)
	}
	if err := os.Chmod(cfgPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := configFilePermissionProblem(cfgPath); got == "" {
		t.Fatal("expected config permission problem")
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
