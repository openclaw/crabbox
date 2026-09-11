package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPhalaOrdinarySources(t *testing.T) {
	clearConfigEnv(t)
	wantDefaults := PhalaConfig{CLIPath: "phala", InstanceType: "tdx.small", WorkRoot: "/var/volatile/crabbox"}
	if got := baseConfig().Phala; !reflect.DeepEqual(got, wantDefaults) {
		t.Fatalf("defaults %#v", got)
	}
	fields := []struct {
		field, key, env string
		path            bool
	}{{"CLIPath", "cli", "CLI", true}, {"InstanceType", "instanceType", "INSTANCE_TYPE", false}, {"WorkRoot", "workRoot", "WORK_ROOT", false}, {"NodeID", "nodeId", "NODE_ID", false}, {"Compose", "compose", "COMPOSE", true}}
	for _, source := range []string{"file", "env"} {
		for _, value := range []string{"", "same", " padded ", "~/ordinary"} {
			t.Run(source+"/"+value, func(t *testing.T) {
				clearConfigEnv(t)
				home := t.TempDir()
				t.Setenv("HOME", home)
				cfg := Config{}
				for _, f := range fields {
					v := "same"
					if f.path {
						v = "~/inherited"
					}
					reflect.ValueOf(&cfg.Phala).Elem().FieldByName(f.field).SetString(v)
				}
				want := cfg.Phala
				for _, f := range fields {
					v := reflect.ValueOf(&want).Elem().FieldByName(f.field)
					if value != "" {
						v.SetString(value)
					}
					if f.path && (source == "env" || value != "") && strings.HasPrefix(v.String(), "~/") {
						v.SetString(filepath.Join(home, strings.TrimPrefix(v.String(), "~/")))
					}
				}
				inputSource := configInputUser
				if source == "file" {
					body := map[string]any{}
					for _, f := range fields {
						body[f.key] = value
					}
					data, err := yaml.Marshal(map[string]any{"phala": body})
					if err != nil {
						t.Fatal(err)
					}
					var file fileConfig
					if err := yaml.Unmarshal(data, &file); err != nil {
						t.Fatal(err)
					}
					original := *file.Phala
					if err := applyFileConfig(&cfg, file); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(*file.Phala, original) {
						t.Fatal("input changed")
					}
				} else {
					inputSource = configInputEnvironment
					for _, f := range fields {
						t.Setenv("CRABBOX_PHALA_"+f.env, value)
					}
					if err := applyEnv(&cfg); err != nil {
						t.Fatal(err)
					}
				}
				var ledger configInputLedger
				if value != "" {
					ledger = ledger.withInput("phala", inputSource, configInputValue)
				}
				if !reflect.DeepEqual(cfg.Phala, want) || PhalaInstanceTypeWasExplicit(cfg) != (value != "") || !reflect.DeepEqual(cfg.inputProvenance, ledger) {
					t.Fatalf("source %#v ledger %#v", cfg.Phala, cfg.inputProvenance)
				}
			})
		}
	}
	for _, source := range []string{"file", "env"} {
		for _, raw := range []string{"null", "false", "true", "invalid", ""} {
			if source == "file" && (raw == "invalid" || raw == "") {
				continue
			}
			t.Run(source+"/pointer/"+raw, func(t *testing.T) {
				clearConfigEnv(t)
				prior := true
				cfg := Config{Phala: PhalaConfig{Attest: &prior}}
				var input *bool
				accepted := raw == "true" || raw == "false"
				inputSource := configInputUser
				if source == "file" {
					var file fileConfig
					if err := yaml.Unmarshal([]byte("phala: {attest: "+raw+"}"), &file); err != nil {
						t.Fatal(err)
					}
					input = file.Phala.Attest
					if err := applyFileConfig(&cfg, file); err != nil {
						t.Fatal(err)
					}
					if input != nil && *input != (raw == "true") {
						t.Fatal("file bool mutated")
					}
				} else {
					inputSource = configInputEnvironment
					t.Setenv("CRABBOX_PHALA_ATTEST", raw)
					if err := applyEnv(&cfg); err != nil {
						t.Fatal(err)
					}
				}
				var ledger configInputLedger
				if accepted {
					ledger = ledger.withInput("phala", inputSource, configInputValue)
					if cfg.Phala.Attest == nil || cfg.Phala.Attest == &prior || cfg.Phala.Attest == input || *cfg.Phala.Attest != (raw == "true") {
						t.Fatal("accepted bool must be independent fresh copy")
					}
				} else if cfg.Phala.Attest != &prior {
					t.Fatal("ignored bool replaced prior pointer")
				}
				if !prior || !reflect.DeepEqual(cfg.inputProvenance, ledger) {
					t.Fatal("prior value or accepted facts changed")
				}
			})
		}
	}
}

func TestPhalaOrdinaryWriterAndRuntimeShape(t *testing.T) {
	for _, raw := range []string{"null", "false", "true"} {
		path := isolatedConfigPath(t)
		body := "phala: {cli: '~/ordinary', instanceType: ' padded ', workRoot: '', nodeId: '', compose: '', attest: " + raw + "}"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
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
		want := map[string]any{"cli": "~/ordinary", "instanceType": " padded "}
		if raw != "null" {
			want["attest"] = raw == "true"
		}
		if !reflect.DeepEqual(got, map[string]any{"phala": want}) {
			t.Fatalf("writer %#v", got)
		}
		cfg := PhalaConfig{}
		if raw != "null" {
			v := raw == "true"
			cfg.Attest = &v
		}
		data, err = json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var runtimeJSON map[string]any
		if err := json.Unmarshal(data, &runtimeJSON); err != nil {
			t.Fatal(err)
		}
		v, present := runtimeJSON["Attest"]
		if !present || len(runtimeJSON) != 6 || (raw == "null" && v != nil) || (raw != "null" && v != (raw == "true")) {
			t.Fatalf("runtime JSON %s", data)
		}
		data, err = yaml.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var runtimeYAML map[string]any
		if err := yaml.Unmarshal(data, &runtimeYAML); err != nil {
			t.Fatal(err)
		}
		v, present = runtimeYAML["attest"]
		if !present || len(runtimeYAML) != 6 || (raw == "null" && v != nil) || (raw != "null" && v != (raw == "true")) {
			t.Fatalf("runtime YAML %s", data)
		}
	}
}

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
