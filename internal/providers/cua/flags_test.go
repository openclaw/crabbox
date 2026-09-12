package cua

import (
	"flag"
	"strconv"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderFlagsApplyAndValidate(t *testing.T) {
	cfg := core.Config{Provider: providerName, Cua: core.CuaConfig{
		Image:             core.CuaConfigDefaultImage,
		Kind:              core.CuaConfigDefaultKind,
		Workdir:           core.CuaConfigDefaultWorkdir,
		ExecTimeoutSecs:   600,
		BridgeCommand:     core.CuaConfigDefaultBridgeCommand,
		SDKPackage:        core.CuaConfigDefaultSDKPackage,
		SDKImport:         core.CuaConfigDefaultSDKImport,
		SDKFallbackImport: core.CuaConfigDefaultSDKFallbackImport,
	}}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := Provider{}.RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--cua-api-url", "https://API.CUA.EXAMPLE:443/v1/",
		"--cua-image", "ubuntu:24.04",
		"--cua-kind", "vm",
		"--cua-region", "us-west",
		"--cua-workdir", "/workspace/app",
		"--cua-vcpus", "4",
		"--cua-memory-mb", "8192",
		"--cua-disk-gb", "40",
		"--cua-startup-timeout-secs", "300",
		"--cua-exec-timeout-secs", "120",
		"--cua-bridge-command", "python3.12",
		"--cua-sdk-package", "cua",
		"--cua-sdk-import", "cua",
		"--cua-sdk-fallback-import", "cua_sandbox",
	}); err != nil {
		t.Fatal(err)
	}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Cua.APIURL != "https://API.CUA.EXAMPLE:443/v1/" ||
		cfg.Cua.Image != "ubuntu:24.04" ||
		cfg.Cua.Kind != "vm" ||
		cfg.Cua.Region != "us-west" ||
		cfg.Cua.Workdir != "/workspace/app" ||
		cfg.Cua.VCPUs != 4 ||
		cfg.Cua.MemoryMB != 8192 ||
		cfg.Cua.DiskGB != 40 ||
		cfg.Cua.StartupTimeoutSecs != 300 ||
		cfg.Cua.ExecTimeoutSecs != 120 ||
		cfg.Cua.BridgeCommand != "python3.12" ||
		cfg.Cua.SDKPackage != "cua" ||
		cfg.Cua.SDKImport != "cua" ||
		cfg.Cua.SDKFallbackImport != "cua_sandbox" {
		t.Fatalf("cfg.Cua=%#v", cfg.Cua)
	}
}

func TestValidateProviderConfigRejectsUnsafeValues(t *testing.T) {
	base := core.CuaConfig{
		Image:             core.CuaConfigDefaultImage,
		Kind:              core.CuaConfigDefaultKind,
		Workdir:           core.CuaConfigDefaultWorkdir,
		BridgeCommand:     core.CuaConfigDefaultBridgeCommand,
		SDKPackage:        core.CuaConfigDefaultSDKPackage,
		SDKImport:         core.CuaConfigDefaultSDKImport,
		SDKFallbackImport: core.CuaConfigDefaultSDKFallbackImport,
	}
	tests := []struct {
		name string
		edit func(*core.CuaConfig)
	}{
		{name: "api-url-userinfo", edit: func(cfg *core.CuaConfig) { cfg.APIURL = "https://token@example.test" }},
		{name: "api-url-query", edit: func(cfg *core.CuaConfig) { cfg.APIURL = "https://api.example.test?token=abc" }},
		{name: "api-url-fragment", edit: func(cfg *core.CuaConfig) { cfg.APIURL = "https://api.example.test/#frag" }},
		{name: "api-url-http", edit: func(cfg *core.CuaConfig) { cfg.APIURL = "http://api.example.test" }},
		{name: "kind", edit: func(cfg *core.CuaConfig) { cfg.Kind = "desktop" }},
		{name: "vcpus", edit: func(cfg *core.CuaConfig) { cfg.VCPUs = -1 }},
		{name: "memory", edit: func(cfg *core.CuaConfig) { cfg.MemoryMB = -1 }},
		{name: "disk", edit: func(cfg *core.CuaConfig) { cfg.DiskGB = -1 }},
		{name: "startup", edit: func(cfg *core.CuaConfig) { cfg.StartupTimeoutSecs = -1 }},
		{name: "exec", edit: func(cfg *core.CuaConfig) { cfg.ExecTimeoutSecs = -1 }},
		{name: "workdir-relative", edit: func(cfg *core.CuaConfig) { cfg.Workdir = "relative" }},
		{name: "workdir-broad", edit: func(cfg *core.CuaConfig) { cfg.Workdir = "/workspace" }},
		{name: "workdir-outside", edit: func(cfg *core.CuaConfig) { cfg.Workdir = "/tmp/app" }},
		{name: "bridge-command-empty", edit: func(cfg *core.CuaConfig) { cfg.BridgeCommand = "" }},
		{name: "sdk-package-empty", edit: func(cfg *core.CuaConfig) { cfg.SDKPackage = "" }},
		{name: "sdk-import-empty", edit: func(cfg *core.CuaConfig) { cfg.SDKImport = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.edit(&cfg)
			err := validateProviderConfig(core.Config{Cua: cfg})
			if err == nil {
				t.Fatalf("validateProviderConfig(%#v) succeeded", cfg)
			}
			if strings.Contains(err.Error(), "token=abc") {
				t.Fatalf("error leaked URL query secret: %v", err)
			}
		})
	}
}

func TestValidateProviderConfigRejectsOverflowingExecTimeout(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("int cannot represent a duration-overflowing seconds value")
	}
	tooLarge := maxBridgeTimeoutSeconds + 1
	cfg := testConfig()
	cfg.Cua.ExecTimeoutSecs = int(tooLarge)
	if err := validateProviderConfig(cfg); err == nil || !strings.Contains(err.Error(), "maximum safe duration") {
		t.Fatalf("validateProviderConfig err=%v", err)
	}
}

func TestValidateProviderConfigAllowsLoopbackAPIURL(t *testing.T) {
	cfg := core.Config{Cua: core.CuaConfig{
		APIURL:            "http://localhost:8080/v1/",
		Image:             core.CuaConfigDefaultImage,
		Kind:              core.CuaConfigDefaultKind,
		Workdir:           core.CuaConfigDefaultWorkdir,
		BridgeCommand:     core.CuaConfigDefaultBridgeCommand,
		SDKPackage:        core.CuaConfigDefaultSDKPackage,
		SDKImport:         core.CuaConfigDefaultSDKImport,
		SDKFallbackImport: core.CuaConfigDefaultSDKFallbackImport,
	}}
	if err := validateProviderConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestCUAAPIURLStripsSDKVersionPrefix(t *testing.T) {
	cfg := testConfig()
	for input, want := range map[string]string{
		"https://api.cua.example/v1/":       "https://api.cua.example",
		"https://proxy.example/cua/v1":      "https://proxy.example/cua",
		"http://localhost:8080/custom-path": "http://localhost:8080/custom-path",
	} {
		cfg.Cua.APIURL = input
		got, err := cuaAPIURL(cfg)
		if err != nil {
			t.Fatalf("cuaAPIURL(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("cuaAPIURL(%q)=%q want %q", input, got, want)
		}
	}
}

func TestProviderRejectsGenericClassAndTypeFlags(t *testing.T) {
	for _, args := range [][]string{{"--class", "large"}, {"--type", "gpu"}} {
		cfg := core.Config{Provider: providerName, Cua: core.CuaConfig{
			Image:             core.CuaConfigDefaultImage,
			Kind:              core.CuaConfigDefaultKind,
			Workdir:           core.CuaConfigDefaultWorkdir,
			BridgeCommand:     core.CuaConfigDefaultBridgeCommand,
			SDKPackage:        core.CuaConfigDefaultSDKPackage,
			SDKImport:         core.CuaConfigDefaultSDKImport,
			SDKFallbackImport: core.CuaConfigDefaultSDKFallbackImport,
		}}
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.String("class", "", "")
		fs.String("type", "", "")
		values := Provider{}.RegisterFlags(fs, cfg)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		if err := (Provider{}).ApplyFlags(&cfg, fs, values); err == nil {
			t.Fatalf("ApplyFlags(%v) succeeded", args)
		}
	}
}

func TestProviderFlagPresenceBeforeValidation(t *testing.T) {
	cfg := testConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterProviderFlags(fs, cfg)
	cfg.Cua = core.CuaConfig{APIURL: "https://example.invalid/later", Image: "later", Kind: "vm", Region: "later", Workdir: "/workspace/later", VCPUs: 1, MemoryMB: 2, DiskGB: 3, StartupTimeoutSecs: 4, ExecTimeoutSecs: 5, BridgeCommand: "python3", SDKPackage: "cua", SDKImport: "cua", SDKFallbackImport: "fallback"}
	before := cfg.Cua
	if err := ApplyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Cua != before {
		t.Fatalf("unvisited flags changed config: %#v", cfg.Cua)
	}
	if err := fs.Parse([]string{"--cua-api-url=", "--cua-image=", "--cua-kind=", "--cua-region=", "--cua-workdir=", "--cua-vcpus=0", "--cua-memory-mb=0", "--cua-disk-gb=0", "--cua-startup-timeout-secs=0", "--cua-exec-timeout-secs=0", "--cua-bridge-command=", "--cua-sdk-package=", "--cua-sdk-import=", "--cua-sdk-fallback-import="}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyProviderFlags(&cfg, fs, values); err == nil {
		t.Fatal("expected empty bridge validation failure")
	}
	if cfg.Cua != (core.CuaConfig{}) {
		t.Fatalf("explicit zero values not copied before validation: %#v", cfg.Cua)
	}
}

func TestProviderSizingGuardPrecedesValuesAssertion(t *testing.T) {
	for _, provider := range []string{"cua", " CUA "} {
		for _, args := range [][]string{nil, {"--class=large"}, {"--type=vm"}, {"--type=vm", "--class=large"}} {
			cfg := testConfig()
			cfg.Provider = provider
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.String("class", "", "")
			fs.String("type", "", "")
			registered := RegisterProviderFlags(fs, cfg)
			if err := fs.Parse(append(args, "--cua-image=changed")); err != nil {
				t.Fatal(err)
			}
			for _, values := range []any{nil, struct{}{}, registered} {
				if len(args) == 0 && values == registered {
					continue
				}
				before := cfg.Cua
				err := ApplyProviderFlags(&cfg, fs, values)
				if len(args) == 0 {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					want := "--class is not supported for provider=cua; use --cua-vcpus and --cua-memory-mb"
					if len(args) == 1 && args[0] == "--type=vm" {
						want = "--type is not supported for provider=cua; use --cua-image and --cua-kind"
					}
					if err == nil || err.Error() != want {
						t.Fatalf("provider=%q args=%v values=%T error=%v, want %q", provider, args, values, err, want)
					}
				}
				if cfg.Cua != before {
					t.Fatal("guard or wrong values copied flags")
				}
			}
		}
	}
}
