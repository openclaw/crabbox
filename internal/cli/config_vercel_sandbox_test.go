package cli

import (
	"io"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// These cases characterize the handwritten loader before its wiring is generated.
func TestVercelSandboxConfigPresence(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, tc := range []struct {
			name, body string
			clear      bool
		}{
			{"omitted", "{}", false},
			{"null", "{runtime: null, vcpus: null, persistent: null, ports: null}", false},
			{"explicit zero values", `{runtime: "", vcpus: 0, persistent: false, ports: []}`, true},
		} {
			t.Run(tc.name+map[bool]string{true: " user", false: " repo"}[trusted], func(t *testing.T) {
				cfg := baseConfig()
				cfg.VercelSandbox.VCPUs = 2
				cfg.VercelSandbox.Persistent = true
				cfg.VercelSandbox.Ports = []string{"3000"}
				before := cfg.VercelSandbox
				var file fileConfig
				if err := yaml.Unmarshal([]byte("vercelSandbox: "+tc.body), &file); err != nil {
					t.Fatal(err)
				}
				if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
					t.Fatal(err)
				}
				if tc.clear {
					if cfg.VercelSandbox.Runtime != "" || cfg.VercelSandbox.VCPUs != 0 || cfg.VercelSandbox.Persistent || len(cfg.VercelSandbox.Ports) != 0 {
						t.Fatalf("explicit values not applied: %#v", cfg.VercelSandbox)
					}
				} else if !reflect.DeepEqual(before, cfg.VercelSandbox) {
					t.Fatalf("absent values changed config: %#v", cfg.VercelSandbox)
				}
			})
		}
	}
}

func TestVercelSandboxConfigFileAndEnvironmentPrecedence(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.VercelSandbox.Runtime != "node24" {
		t.Fatal("compiled default changed")
	}
	for _, layer := range []struct {
		source  providerSelectionSource
		runtime string
		trusted bool
	}{
		{providerSelectionUserConfig, "node22", true}, {providerSelectionRepoConfig, "python3.13", false},
	} {
		var file fileConfig
		if err := yaml.Unmarshal([]byte("provider: vercel-sandbox\nvercelSandbox:\n  runtime: "+layer.runtime), &file); err != nil {
			t.Fatal(err)
		}
		if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, layer.trusted, layer.source); err != nil {
			t.Fatal(err)
		}
		if cfg.VercelSandbox.Runtime != layer.runtime || cfg.providerSelectionSource != layer.source || !providerSelectionIsActionable(cfg) {
			t.Fatalf("layer %s not applied", layer.source)
		}
	}
	t.Setenv("CRABBOX_PROVIDER", "vercel-sandbox")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_RUNTIME", "node26")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.VercelSandbox.Runtime != "node26" || cfg.providerSelectionSource != providerSelectionEnvironment {
		t.Fatal("environment did not win")
	}
}

func TestVercelSandboxConfigEnvironmentParsing(t *testing.T) {
	for _, tc := range []struct {
		key, value string
		wantErr    string
		want       func(VercelSandboxConfig) bool
	}{
		{"RUNTIME", "", "", func(c VercelSandboxConfig) bool { return c.Runtime == "node24" }},
		{"RUNTIME", " ", "", func(c VercelSandboxConfig) bool { return c.Runtime == " " }},
		{"VCPUS", "bad", "", func(c VercelSandboxConfig) bool { return c.VCPUs == 2 }},
		{"VCPUS", "0", "", func(c VercelSandboxConfig) bool { return c.VCPUs == 0 }},
		{"PERSISTENT", "bad", "", func(c VercelSandboxConfig) bool { return c.Persistent }},
		{"PERSISTENT", " OFF ", "", func(c VercelSandboxConfig) bool { return !c.Persistent }},
		{"PORTS", "", "", func(c VercelSandboxConfig) bool { return reflect.DeepEqual(c.Ports, []string{"3000"}) }},
		{"PORTS", " , ", "", func(c VercelSandboxConfig) bool { return len(c.Ports) == 0 }},
		{"PORTS", " 3000, , 3000,8080 ", "", func(c VercelSandboxConfig) bool { return reflect.DeepEqual(c.Ports, []string{"3000", "3000", "8080"}) }},
		{"EXEC_TIMEOUT_SECS", "0", "", func(c VercelSandboxConfig) bool { return c.ExecTimeoutSecs == 0 }},
		{"EXEC_TIMEOUT_SECS", "bad", "must be an integer", nil},
		{"TIMEOUT_SECS", "-1", "must be non-negative", nil},
		{"TIMEOUT_SECS", " 1 ", "must be an integer", nil},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.VercelSandbox.VCPUs = 2
			cfg.VercelSandbox.Persistent = true
			cfg.VercelSandbox.Ports = []string{"3000"}
			t.Setenv("CRABBOX_VERCEL_SANDBOX_"+tc.key, tc.value)
			err := applyEnv(&cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tc.want(cfg.VercelSandbox) {
				t.Fatalf("unexpected config: %#v", cfg.VercelSandbox)
			}
		})
	}
}

func TestVercelSandboxConfigRejectsInvalidFileValues(t *testing.T) {
	for _, body := range []string{"timeoutSecs: -1", "execTimeoutSecs: -1", "vcpus: nope", "persistent: nope", "ports: nope"} {
		t.Run(body, func(t *testing.T) {
			cfg := baseConfig()
			var file fileConfig
			err := yaml.Unmarshal([]byte("vercelSandbox:\n  "+body), &file)
			if err == nil {
				err = applyFileConfig(&cfg, file)
			}
			if err == nil {
				t.Fatal("invalid file accepted")
			}
		})
	}
}

func TestVercelSandboxConfigCannotIntroduceCredentialsOrDestinations(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		cfg := baseConfig()
		before := cfg.VercelSandbox
		var file fileConfig
		if err := yaml.Unmarshal([]byte("vercelSandbox:\n  token: fake-token\n  authToken: fake-auth\n  apiUrl: https://untrusted.example\n  bridgeUrl: https://untrusted.example"), &file); err != nil {
			t.Fatal(err)
		}
		if err := applyFileConfigWithTrust(&cfg, file, trusted); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, cfg.VercelSandbox) {
			t.Fatal("unsupported auth/destination keys changed config")
		}
	}
	for _, name := range []string{"Token", "AuthToken", "OIDCToken", "APIURL", "BridgeURL", "Endpoint"} {
		if _, ok := reflect.TypeFor[VercelSandboxConfig]().FieldByName(name); ok {
			t.Fatalf("runtime config exposes %s", name)
		}
		if _, ok := reflect.TypeFor[fileVercelSandboxConfig]().FieldByName(name); ok {
			t.Fatalf("file config exposes %s", name)
		}
	}
}

func TestVercelSandboxFlagsDoNotSelectProvider(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "implicit provider", true: "explicit provider"}[explicit], func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Provider = "ssh"
			cfg.VercelSandbox.Runtime = "node22"
			beforeSource := cfg.providerSelectionSource
			fs := newFlagSet("test", io.Discard)
			fs.String("provider", "", "")
			values := registerProviderFlags(fs, baseConfig())
			// Internal CLI tests use stand-in providers to avoid an import cycle.
			// Provider tests separately exercise the real Vercel validation wrapper.
			values["vercel-sandbox"] = RegisterVercelSandboxConfigFlags(fs, baseConfig().VercelSandbox)
			args := []string{"--vercel-sandbox-runtime=invalid"}
			if explicit {
				args = append(args, "--provider=ssh")
			}
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			if err := applyProviderFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if cfg.VercelSandbox.Runtime != "node22" || cfg.Provider != "ssh" {
				t.Fatal("nonselected provider flag applied")
			}
			wantSource := beforeSource
			if explicit {
				wantSource = providerSelectionFlag
			}
			if cfg.providerExplicit != explicit || cfg.providerSelectionSource != wantSource {
				t.Fatal("provider selection provenance changed")
			}
		})
	}
}
