package cua

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

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
