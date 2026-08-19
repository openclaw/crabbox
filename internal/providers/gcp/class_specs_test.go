package gcp

import (
	"reflect"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestClassSpecs(t *testing.T) {
	want := []core.ClassSpec{
		{Class: "tiny", Type: "c4-standard-4", VCPUs: 4, MemoryGB: 15},
		{Class: "small", Type: "c4-standard-8", VCPUs: 8, MemoryGB: 30},
		{Class: "standard", Type: "c4-standard-32", VCPUs: 32, MemoryGB: 120},
		{Class: "fast", Type: "c4-standard-64", VCPUs: 64, MemoryGB: 240},
		{Class: "large", Type: "c4-standard-96", VCPUs: 96, MemoryGB: 360},
		{Class: "beast", Type: "c4-standard-192", VCPUs: 192, MemoryGB: 720},
	}
	if got := (Provider{}).ClassSpecs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ClassSpecs()=%#v want %#v", got, want)
	}
}

func TestClassProfilesCoverCanonicalClasses(t *testing.T) {
	profiles := (Provider{}).ClassProfiles()
	if len(profiles) != len(core.CanonicalProviderClasses()) {
		t.Fatalf("ClassProfiles len=%d want %d", len(profiles), len(core.CanonicalProviderClasses()))
	}
	for _, profile := range profiles {
		if profile.Primary.Type == "" || profile.Fallbacks == nil {
			t.Fatalf("incomplete profile: %#v", profile)
		}
	}
}

func TestTinyAndSmallCandidateMappings(t *testing.T) {
	tests := map[string][]string{
		"tiny":  {"c4-standard-4", "c3-standard-4", "n2-standard-4", "n2d-standard-4"},
		"small": {"c4-standard-8", "c3-standard-8", "n2-standard-8", "n2d-standard-8", "c4-standard-4"},
	}
	for class, want := range tests {
		if got := gcpMachineTypeCandidatesForClass(class); !reflect.DeepEqual(got, want) {
			t.Errorf("class=%s candidates=%v want %v", class, got, want)
		}
	}
}

func TestMachineShape(t *testing.T) {
	tests := []struct {
		name       string
		machine    string
		wantVCPUs  int
		wantMemory float64
	}{
		{name: "C4 standard", machine: "c4-standard-32", wantVCPUs: 32, wantMemory: 120},
		{name: "C3 standard", machine: "c3-standard-22", wantVCPUs: 22, wantMemory: 88},
		{name: "N2 standard", machine: "n2-standard-32", wantVCPUs: 32, wantMemory: 128},
		{name: "N2D standard", machine: "n2d-standard-32", wantVCPUs: 32, wantMemory: 128},
		{name: "known vCPU only", machine: "c4-highcpu-32", wantVCPUs: 32},
		{name: "unknown standard family", machine: "future-standard-32", wantVCPUs: 32},
		{name: "fractional GB", machine: "c4-standard-1", wantVCPUs: 1, wantMemory: 3.75},
		{name: "unparseable vCPU", machine: "c4-standard-many"},
		{name: "missing suffix", machine: "c4-standard"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotVCPUs, gotMemory := machineShape(test.machine)
			if gotVCPUs != test.wantVCPUs || gotMemory != test.wantMemory {
				t.Fatalf("machineShape(%q)=(%d,%g) want (%d,%g)", test.machine, gotVCPUs, gotMemory, test.wantVCPUs, test.wantMemory)
			}
		})
	}
}
