package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPhalaConfigDefaults(t *testing.T) {
	cfg := baseConfig()
	got := cfg.Phala
	if got.CLIPath != "phala" {
		t.Fatalf("default CLIPath=%q want phala", got.CLIPath)
	}
	if got.InstanceType != "tdx.small" {
		t.Fatalf("default InstanceType=%q want tdx.small", got.InstanceType)
	}
	if got.WorkRoot != "/var/volatile/crabbox" {
		t.Fatalf("default WorkRoot=%q want /var/volatile/crabbox", got.WorkRoot)
	}
	if got.NodeID != "" || got.Compose != "" {
		t.Fatalf("unexpected non-empty default node/compose: %#v", got)
	}
	if ClassWasExplicit(cfg) || PhalaInstanceTypeWasExplicit(cfg) {
		t.Fatal("defaults were marked explicit")
	}
}

func TestPhalaFileConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
class: fast
phala:
  cli: /opt/phala
  instanceType: tdx.large
  workRoot: /workspace
  nodeId: node-42
  compose: ./compose.yml
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", path)
	t.Setenv("CRABBOX_PROVIDER", "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Phala
	if got.CLIPath != "/opt/phala" || got.InstanceType != "tdx.large" ||
		got.WorkRoot != "/workspace" || got.NodeID != "node-42" {
		t.Fatalf("phala=%#v", got)
	}
	if got.Compose == "" {
		t.Fatalf("compose not loaded from trusted file config: %#v", got)
	}
	if !ClassWasExplicit(cfg) || !PhalaInstanceTypeWasExplicit(cfg) {
		t.Fatal("file class or Phala instance type was not marked explicit")
	}
	if !PhalaInstanceTypeOverridesClass(cfg) {
		t.Fatal("same-file Phala instance type did not override class")
	}
}

func TestPhalaEnvConfig(t *testing.T) {
	cfg := baseConfig()
	t.Setenv("CRABBOX_PHALA_CLI", "/opt/phala-env")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "large")
	t.Setenv("CRABBOX_PHALA_INSTANCE_TYPE", "tdx.medium")
	t.Setenv("CRABBOX_PHALA_WORK_ROOT", "/work/env")
	t.Setenv("CRABBOX_PHALA_NODE_ID", "node-env")
	t.Setenv("CRABBOX_PHALA_COMPOSE", "/etc/compose.yml")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	got := cfg.Phala
	if got.CLIPath != "/opt/phala-env" || got.InstanceType != "tdx.medium" ||
		got.WorkRoot != "/work/env" || got.NodeID != "node-env" || got.Compose != "/etc/compose.yml" {
		t.Fatalf("phala=%#v", got)
	}
	if !ClassWasExplicit(cfg) || !PhalaInstanceTypeWasExplicit(cfg) {
		t.Fatal("environment class or Phala instance type was not marked explicit")
	}
	if !PhalaInstanceTypeOverridesClass(cfg) {
		t.Fatal("Phala environment instance type did not override environment class")
	}
}

func TestPhalaEnvironmentClassOverridesFileInstanceType(t *testing.T) {
	cfg := baseConfig()
	file := fileConfig{Phala: &filePhalaConfig{InstanceType: "tdx.large"}}
	if err := applyFileConfigWithTrust(&cfg, file, true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_DEFAULT_CLASS", "fast")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if !ClassWasExplicit(cfg) || !PhalaInstanceTypeWasExplicit(cfg) {
		t.Fatal("expected both selectors to remain explicitly tracked")
	}
	if PhalaInstanceTypeOverridesClass(cfg) {
		t.Fatal("older file Phala instance type overrode environment class")
	}
}

// TestPhalaUntrustedConfigCannotRedirectCLIOrAccount mirrors the namespace
// instance trust split: an untrusted (repo-supplied) config may set the safe
// instanceType/workRoot but must not redirect the CLI binary, pinned node, or
// compose file, which control which credentials and node the deployment uses.
func TestPhalaUntrustedConfigCannotRedirectCLIOrAccount(t *testing.T) {
	cfg := baseConfig()
	cfg.Phala.CLIPath = "/trusted/phala"
	cfg.Phala.NodeID = "trusted-node"
	cfg.Phala.Compose = "/trusted/compose.yml"
	file := fileConfig{Phala: &filePhalaConfig{
		CLIPath:      "/repo/phala",
		InstanceType: "tdx.large",
		WorkRoot:     "/workspace",
		NodeID:       "repo-node",
		Compose:      "/repo/compose.yml",
	}}
	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	got := cfg.Phala
	if got.CLIPath != "/trusted/phala" || got.NodeID != "trusted-node" || got.Compose != "/trusted/compose.yml" {
		t.Fatalf("untrusted account routing applied: %#v", got)
	}
	if got.InstanceType != "tdx.large" || got.WorkRoot != "/workspace" {
		t.Fatalf("safe untrusted settings not applied: %#v", got)
	}
}

func TestPhalaTrustedConfigRedirectsCLIAndAccount(t *testing.T) {
	cfg := baseConfig()
	file := fileConfig{Phala: &filePhalaConfig{
		CLIPath:      "/repo/phala",
		InstanceType: "tdx.large",
		WorkRoot:     "/workspace",
		NodeID:       "repo-node",
		Compose:      "/repo/compose.yml",
	}}
	if err := applyFileConfigWithTrust(&cfg, file, true); err != nil {
		t.Fatal(err)
	}
	got := cfg.Phala
	if got.CLIPath != "/repo/phala" || got.NodeID != "repo-node" ||
		got.InstanceType != "tdx.large" || got.WorkRoot != "/workspace" {
		t.Fatalf("trusted config not fully applied: %#v", got)
	}
	if got.Compose == "" {
		t.Fatalf("trusted compose not applied: %#v", got)
	}
}

func TestPhalaClaimScope(t *testing.T) {
	cfg := baseConfig()
	// No pinned node yields an empty (global) claim scope.
	if got := ProviderClaimScope("phala", cfg); got != "" {
		t.Fatalf("scope without node=%q want empty", got)
	}
	cfg.Phala.NodeID = "  node-7  "
	if got, want := ProviderClaimScope("phala", cfg), "node:node-7"; got != want {
		t.Fatalf("scope=%q want %q", got, want)
	}
}
