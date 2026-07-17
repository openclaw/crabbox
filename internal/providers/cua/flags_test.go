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
		Image:             defaultImage,
		Kind:              defaultKind,
		Workdir:           defaultWorkdir,
		ExecTimeoutSecs:   600,
		BridgeCommand:     defaultBridgeCommand,
		SDKPackage:        defaultSDKPackage,
		SDKImport:         defaultSDKImport,
		SDKFallbackImport: defaultSDKFallbackImport,
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
		Image:             defaultImage,
		Kind:              defaultKind,
		Workdir:           defaultWorkdir,
		BridgeCommand:     defaultBridgeCommand,
		SDKPackage:        defaultSDKPackage,
		SDKImport:         defaultSDKImport,
		SDKFallbackImport: defaultSDKFallbackImport,
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
		Image:             defaultImage,
		Kind:              defaultKind,
		Workdir:           defaultWorkdir,
		BridgeCommand:     defaultBridgeCommand,
		SDKPackage:        defaultSDKPackage,
		SDKImport:         defaultSDKImport,
		SDKFallbackImport: defaultSDKFallbackImport,
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
			Image:             defaultImage,
			Kind:              defaultKind,
			Workdir:           defaultWorkdir,
			BridgeCommand:     defaultBridgeCommand,
			SDKPackage:        defaultSDKPackage,
			SDKImport:         defaultSDKImport,
			SDKFallbackImport: defaultSDKFallbackImport,
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
