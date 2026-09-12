package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestNamespaceInstanceOrdinaryFileMetadata(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	wantDefault := NamespaceInstanceConfig{CLIPath: "nsc", WorkRoot: "/work/crabbox", Bare: true}
	if got := baseConfig().NamespaceInstance; !reflect.DeepEqual(got, wantDefault) {
		t.Fatalf("defaults=%#v want %#v", got, wantDefault)
	}
	if reflect.TypeOf(fileNamespaceInstanceConfig{}).Name() != "fileNamespaceInstanceConfig" {
		t.Fatal("file DTO name")
	}
	for _, tc := range []struct {
		name, list string
		want       []string
	}{{"absent", "", []string{"prior"}}, {"null", "  volumes: null\n", []string{"prior"}}, {"empty", "  volumes: []\n", nil}, {"raw clone", "  volumes: [' a ', a, a]\n", []string{" a ", "a", "a"}}} {
		t.Run(tc.name, func(t *testing.T) {
			document := "namespaceInstance:\n  cli: '~/nsc'\n  machineType: ' 4x8 '\n  duration: 20m\n  region: ' eu '\n  endpoint: https://example.invalid\n  keychain: fixture\n  workRoot: '~/guest'\n  bare: false\n" + tc.list
			var file fileConfig
			if err := yaml.Unmarshal([]byte(document), &file); err != nil {
				t.Fatal(err)
			}
			cfg := baseConfig()
			cfg.NamespaceInstance.TenantID = "tenant-sentinel"
			cfg.NamespaceInstance.Volumes = []string{"prior"}
			generic := cfg.WorkRoot
			if err := applyFileConfig(&cfg, file); err != nil {
				t.Fatal(err)
			}
			want := NamespaceInstanceConfig{CLIPath: filepath.Join(home, "nsc"), MachineType: " 4x8 ", Duration: 20 * time.Minute, Region: " eu ", Endpoint: "https://example.invalid", Keychain: "fixture", TenantID: "tenant-sentinel", Volumes: tc.want, WorkRoot: "~/guest", Bare: false}
			if !reflect.DeepEqual(cfg.NamespaceInstance, want) || cfg.WorkRoot != generic {
				t.Fatalf("file metadata=%#v want %#v", cfg.NamespaceInstance, want)
			}
			if len(file.NamespaceInstance.Volumes) > 0 {
				file.NamespaceInstance.Volumes[0] = "changed"
				if cfg.NamespaceInstance.Volumes[0] != " a " {
					t.Fatal("file list was not cloned")
				}
			}
		})
	}
	for _, raw := range []string{"", "0s", "-1m", " 2m ", "invalid", "2m"} {
		cfg := baseConfig()
		cfg.NamespaceInstance.Duration = time.Minute
		cfg.NamespaceInstance.CLIPath = "~/prior"
		cfg.NamespaceInstance.TenantID = "tenant-sentinel"
		if err := applyFileConfig(&cfg, fileConfig{NamespaceInstance: &fileNamespaceInstanceConfig{Duration: raw}}); err != nil {
			t.Fatal(err)
		}
		want := time.Minute
		if raw == "2m" {
			want = 2 * time.Minute
		}
		if cfg.NamespaceInstance.Duration != want || cfg.NamespaceInstance.CLIPath != "~/prior" || cfg.NamespaceInstance.TenantID != "tenant-sentinel" {
			t.Fatal("tolerant file duration/path absence")
		}
	}
	path := isolatedConfigPath(t)
	input := "namespaceInstance: {duration: 0s, volumes: [], bare: false}\n"
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := readFileConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeUserFileConfig(file); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"namespaceInstance": map[string]any{"duration": "0s", "bare": false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("file DTO serialization=%#v", got)
	}
}

func TestNamespaceInstanceOrdinaryEnvMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  bool
		raw  string
		want []string
	}{{"absent", false, "", []string{"prior"}}, {"empty", true, "", []string{}}, {"spaces", true, "  ", []string{}}, {"commas", true, ", ,", []string{}}, {"none", true, " NoNe ", []string{}}, {"csv", true, " a, ,b,a ", []string{"a", "b", "a"}}} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CRABBOX_NAMESPACE_INSTANCE_VOLUMES", "")
			if err := os.Unsetenv("CRABBOX_NAMESPACE_INSTANCE_VOLUMES"); err != nil {
				t.Fatal(err)
			}
			if tc.set {
				t.Setenv("CRABBOX_NAMESPACE_INSTANCE_VOLUMES", tc.raw)
			}
			for key, value := range map[string]string{"MACHINE_TYPE": " 8x16 ", "DURATION": "3m", "REGION": " us ", "ENDPOINT": "https://example.invalid", "KEYCHAIN": "fixture", "WORK_ROOT": "~/guest", "BARE": "false"} {
				t.Setenv("CRABBOX_NAMESPACE_INSTANCE_"+key, value)
			}
			cfg := baseConfig()
			cfg.NamespaceInstance.CLIPath = "~/prior"
			cfg.NamespaceInstance.TenantID = "tenant-sentinel"
			cfg.NamespaceInstance.Volumes = []string{"prior"}
			generic := cfg.WorkRoot
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			want := NamespaceInstanceConfig{CLIPath: filepath.Join(home, "prior"), MachineType: " 8x16 ", Duration: 3 * time.Minute, Region: " us ", Endpoint: "https://example.invalid", Keychain: "fixture", TenantID: "tenant-sentinel", Volumes: tc.want, WorkRoot: "~/guest", Bare: false}
			if !reflect.DeepEqual(cfg.NamespaceInstance, want) || cfg.WorkRoot != generic {
				t.Fatalf("env metadata=%#v want %#v", cfg.NamespaceInstance, want)
			}
		})
	}
	for _, raw := range []string{"", "0s", "-1m", " 2m ", "invalid", "2m"} {
		t.Run("duration-"+raw, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("CRABBOX_NAMESPACE_INSTANCE_DURATION", raw)
			t.Setenv("CRABBOX_NAMESPACE_INSTANCE_BARE", "invalid")
			cfg := baseConfig()
			cfg.NamespaceInstance.Duration = time.Minute
			cfg.NamespaceInstance.TenantID = "tenant-sentinel"
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			want := time.Minute
			if raw == "2m" {
				want = 2 * time.Minute
			}
			if cfg.NamespaceInstance.Duration != want || !cfg.NamespaceInstance.Bare || cfg.NamespaceInstance.TenantID != "tenant-sentinel" {
				t.Fatal("env tolerant duration/bool")
			}
		})
	}
}

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
