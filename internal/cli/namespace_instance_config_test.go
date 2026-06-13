package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestNamespaceInstanceFileConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
namespaceInstance:
  cli: /opt/nsc
  machineType: 2x4
  duration: 20m
  region: eu
  endpoint: https://api.example.test
  keychain: ci
  volumes:
    - cache:go:/root/.cache/go-build:10Gi
  workRoot: /workspace
  bare: false
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", path)
	t.Setenv("CRABBOX_PROVIDER", "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.NamespaceInstance
	if got.CLIPath != "/opt/nsc" || got.MachineType != "2x4" || got.Duration != 20*time.Minute ||
		got.Region != "eu" || got.Endpoint != "https://api.example.test" || got.Keychain != "ci" ||
		got.WorkRoot != "/workspace" || got.Bare ||
		!reflect.DeepEqual(got.Volumes, []string{"cache:go:/root/.cache/go-build:10Gi"}) {
		t.Fatalf("namespaceInstance=%#v", got)
	}
}

func TestNamespaceInstanceEnvConfig(t *testing.T) {
	cfg := baseConfig()
	t.Setenv("CRABBOX_NAMESPACE_INSTANCE_CLI", "/opt/nsc-env")
	t.Setenv("CRABBOX_NAMESPACE_INSTANCE_MACHINE_TYPE", "8x16")
	t.Setenv("CRABBOX_NAMESPACE_INSTANCE_DURATION", "25m")
	t.Setenv("CRABBOX_NAMESPACE_INSTANCE_REGION", "us")
	t.Setenv("CRABBOX_NAMESPACE_INSTANCE_VOLUMES", "cache:a:/a:1Gi,persistent:b:/b:2Gi")
	t.Setenv("CRABBOX_NAMESPACE_INSTANCE_BARE", "false")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.NamespaceInstance.CLIPath != "/opt/nsc-env" || cfg.NamespaceInstance.MachineType != "8x16" ||
		cfg.NamespaceInstance.Duration != 25*time.Minute || cfg.NamespaceInstance.Region != "us" ||
		cfg.NamespaceInstance.Bare ||
		!reflect.DeepEqual(cfg.NamespaceInstance.Volumes, []string{"cache:a:/a:1Gi", "persistent:b:/b:2Gi"}) {
		t.Fatalf("namespaceInstance=%#v", cfg.NamespaceInstance)
	}
}

func TestNamespaceInstanceUntrustedConfigCannotRedirectCLIOrAccount(t *testing.T) {
	cfg := baseConfig()
	cfg.NamespaceInstance.CLIPath = "/trusted/nsc"
	cfg.NamespaceInstance.Region = "trusted-region"
	cfg.NamespaceInstance.Endpoint = "https://trusted.example.test"
	cfg.NamespaceInstance.Keychain = "trusted-keychain"
	bare := false
	file := fileConfig{NamespaceInstance: &fileNamespaceInstanceConfig{
		CLIPath:     "/repo/nsc",
		MachineType: "8x16",
		Duration:    "25m",
		Region:      "repo-region",
		Endpoint:    "https://repo.example.test",
		Keychain:    "repo-keychain",
		Volumes:     []string{"cache:go:/root/.cache/go-build:10Gi"},
		WorkRoot:    "/workspace",
		Bare:        &bare,
	}}
	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	got := cfg.NamespaceInstance
	if got.CLIPath != "/trusted/nsc" || got.Region != "trusted-region" ||
		got.Endpoint != "https://trusted.example.test" || got.Keychain != "trusted-keychain" {
		t.Fatalf("untrusted account routing applied: %#v", got)
	}
	if got.MachineType != "8x16" || got.Duration != 25*time.Minute ||
		got.WorkRoot != "/workspace" || got.Bare ||
		len(got.Volumes) != 0 {
		t.Fatalf("safe untrusted settings not applied: %#v", got)
	}
}

func TestNamespaceInstanceClaimScope(t *testing.T) {
	cfg := baseConfig()
	cfg.NamespaceInstance.Endpoint = "HTTPS://API.EXAMPLE.TEST/"
	cfg.NamespaceInstance.Region = " eu "
	cfg.NamespaceInstance.Keychain = " ci "
	cfg.NamespaceInstance.TenantID = "tenant_test"
	if got, want := ProviderClaimScope("namespace-instance", cfg), "endpoint:https://api.example.test|region:eu|keychain:ci"; got != want {
		t.Fatalf("scope=%q want %q", got, want)
	}
	cfg.NamespaceInstance.Endpoint = "user:secret@api.example.test/path"
	if got, want := ProviderClaimScope("namespace-instance", cfg), "endpoint:api.example.test/path|region:eu|keychain:ci"; got != want {
		t.Fatalf("schemeless scope=%q want %q", got, want)
	}
}
