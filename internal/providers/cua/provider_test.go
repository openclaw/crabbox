package cua

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCuaConfigShowCompletePassiveSection(t *testing.T) {
	projector, ok := any(Provider{}).(core.ProviderConfigShowProjector)
	if !ok {
		t.Fatal("actual provider has no passive config display section")
	}
	for _, selected := range []string{"other", "cua"} {
		for _, populated := range []bool{false, true} {
			cfg := core.Config{Provider: selected}
			want := core.ProviderConfigShowSection{JSONKey: "cua", TextLabel: "cua", Providers: []string{"cua"}, Fields: []core.ProviderConfigShowField{
				{JSONName: "apiUrl", JSONValue: "", TextName: "api_url", TextValue: "-"},
				{JSONName: "image", JSONValue: "", TextName: "image", TextValue: ""},
				{JSONName: "kind", JSONValue: "", TextName: "kind", TextValue: ""},
				{JSONName: "region", JSONValue: "", TextName: "region", TextValue: ""},
				{JSONName: "workdir", JSONValue: "", TextName: "workdir", TextValue: ""},
				{JSONName: "vcpus", JSONValue: 0, TextName: "vcpus", TextValue: "0"},
				{JSONName: "memoryMB", JSONValue: 0, TextName: "memory_mb", TextValue: "0"},
				{JSONName: "diskGB", JSONValue: 0, TextName: "disk_gb", TextValue: "0"},
				{JSONName: "startupTimeoutSecs", JSONValue: 0, TextName: "startup_timeout_secs", TextValue: "0"},
				{JSONName: "execTimeoutSecs", JSONValue: 0, TextName: "exec_timeout_secs", TextValue: "0"},
				{JSONName: "bridgeCommand", JSONValue: "", TextName: "bridge_command", TextValue: ""},
				{JSONName: "sdkPackage", JSONValue: "", TextName: "sdk_package", TextValue: ""},
				{JSONName: "sdkImport", JSONValue: "", TextName: "sdk_import", TextValue: ""},
				{JSONName: "sdkFallbackImport", JSONValue: "", TextName: "sdk_fallback_import", TextValue: ""},
			}}
			if populated {
				cfg.Cua = core.CuaConfig{APIURL: "https://example.invalid/path?view=compact#part", Image: " raw-image ", Kind: " raw-kind ", Region: "", Workdir: " raw-workdir ", VCPUs: 0, MemoryMB: -2, DiskGB: 7, StartupTimeoutSecs: 0, ExecTimeoutSecs: 13, BridgeCommand: " ordinary-bridge ", SDKPackage: " ordinary-package ", SDKImport: " ordinary.import ", SDKFallbackImport: " ordinary.fallback "}
				want.Fields = []core.ProviderConfigShowField{
					{JSONName: "apiUrl", JSONValue: "https://example.invalid/path", TextName: "api_url", TextValue: "https://example.invalid/path"},
					{JSONName: "image", JSONValue: " raw-image ", TextName: "image", TextValue: " raw-image "},
					{JSONName: "kind", JSONValue: " raw-kind ", TextName: "kind", TextValue: " raw-kind "},
					{JSONName: "region", JSONValue: "", TextName: "region", TextValue: ""},
					{JSONName: "workdir", JSONValue: " raw-workdir ", TextName: "workdir", TextValue: " raw-workdir "},
					{JSONName: "vcpus", JSONValue: 0, TextName: "vcpus", TextValue: "0"},
					{JSONName: "memoryMB", JSONValue: -2, TextName: "memory_mb", TextValue: "-2"},
					{JSONName: "diskGB", JSONValue: 7, TextName: "disk_gb", TextValue: "7"},
					{JSONName: "startupTimeoutSecs", JSONValue: 0, TextName: "startup_timeout_secs", TextValue: "0"},
					{JSONName: "execTimeoutSecs", JSONValue: 13, TextName: "exec_timeout_secs", TextValue: "13"},
					{JSONName: "bridgeCommand", JSONValue: " ordinary-bridge ", TextName: "bridge_command", TextValue: " ordinary-bridge "},
					{JSONName: "sdkPackage", JSONValue: " ordinary-package ", TextName: "sdk_package", TextValue: " ordinary-package "},
					{JSONName: "sdkImport", JSONValue: " ordinary.import ", TextName: "sdk_import", TextValue: " ordinary.import "},
					{JSONName: "sdkFallbackImport", JSONValue: " ordinary.fallback ", TextName: "sdk_fallback_import", TextValue: " ordinary.fallback "},
				}
			}
			before := cfg
			got := projector.ConfigShowSection(cfg)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("passive section got %#v want %#v", got, want)
			}
			if !reflect.DeepEqual(cfg, before) {
				t.Fatal("projection mutated supplied config")
			}
		}
	}
}

func TestProviderSpecAndRegistration(t *testing.T) {
	p := Provider{}
	if p.Name() != providerName {
		t.Fatalf("Name=%q want %q", p.Name(), providerName)
	}
	if len(p.Aliases()) != 0 {
		t.Fatalf("Aliases=%v, want none", p.Aliases())
	}
	spec := p.Spec()
	if spec.Name != providerName || spec.Family != providerName {
		t.Fatalf("spec identity=%#v", spec)
	}
	if spec.Kind != core.ProviderKindServiceControl {
		t.Fatalf("Kind=%q want service-control", spec.Kind)
	}
	if len(spec.Targets) != 3 || spec.Targets[0].OS != core.TargetLinux || spec.Targets[1].OS != core.TargetMacOS || spec.Targets[2].OS != core.TargetWindows || spec.Targets[2].WindowsMode != core.WindowsModeNormal {
		t.Fatalf("Targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{
		core.FeatureCleanup,
		core.FeatureArchiveSync,
		core.FeatureSSH,
		core.FeatureDesktop,
		core.FeatureBrowser,
		core.FeatureCode,
		core.FeatureTailscale,
		core.FeatureURLBridge,
		core.FeatureCheckpoint,
		core.FeatureFork,
		core.FeatureSnapshot,
		core.FeatureCacheVolume,
		core.FeatureRunSession,
		core.FeatureRunArtifacts,
		core.FeatureRunDownloads,
		core.FeatureMCP,
	} {
		if spec.Features.Has(feature) {
			t.Fatalf("Features=%#v unexpectedly advertises %s", spec.Features, feature)
		}
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("Coordinator=%q want never", spec.Coordinator)
	}
	got, err := core.ProviderFor(providerName)
	if err != nil {
		t.Fatalf("ProviderFor(cua): %v", err)
	}
	if got.Name() != providerName {
		t.Fatalf("ProviderFor(cua).Name=%q", got.Name())
	}
	for _, alias := range []string{"cua-cloud", "cua-sandbox", "trycua"} {
		if got, err := core.ProviderFor(alias); err == nil && got.Name() == providerName {
			t.Fatalf("alias %q unexpectedly resolves to cua", alias)
		}
	}
}

func TestProviderMetadataEntry(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "providers", "provider-metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]struct {
		Category  string `json:"category"`
		Substrate string `json:"substrate"`
		SSH       string `json:"ssh"`
		Sync      string `json:"sync"`
		GPU       string `json:"gpu"`
		Cleanup   string `json:"cleanup"`
		BestFit   string `json:"bestFit"`
		Caveat    string `json:"caveat"`
		Docs      string `json:"docs"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	entry, ok := metadata[providerName]
	if !ok {
		t.Fatalf("provider metadata missing %q", providerName)
	}
	if entry.Category != "service-control" || entry.SSH != "no" || entry.Sync != "none" || entry.GPU != "unknown" || entry.Docs != "cua.md" {
		t.Fatalf("unexpected cua metadata: %#v", entry)
	}
	if entry.Substrate == "" || entry.Cleanup == "" || !strings.Contains(entry.BestFit, "diagnostics") || !strings.Contains(entry.Caveat, "read-only") {
		t.Fatalf("incomplete cua metadata: %#v", entry)
	}
}
