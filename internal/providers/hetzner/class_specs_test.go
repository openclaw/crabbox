package hetzner

import (
	"reflect"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestPrimaryClassProjection(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  core.Config
		want string
	}{
		{name: "matched", cfg: core.Config{Class: "standard", TargetOS: core.TargetLinux, Architecture: core.ArchitectureAMD64}, want: "ccx33"},
		{name: "unsupported target", cfg: core.Config{Class: "standard", TargetOS: core.TargetMacOS}},
		{name: "unsupported architecture", cfg: core.Config{Class: "standard", TargetOS: core.TargetLinux, Architecture: core.ArchitectureARM64}},
		{name: "custom raw fallback", cfg: core.Config{Class: " custom-shape "}, want: " custom-shape "},
		{name: "canonical spelling remains exact", cfg: core.Config{Class: " STANDARD "}, want: " STANDARD "},
		{name: "explicit type trims and overrides", cfg: core.Config{Class: "standard", TargetOS: core.TargetMacOS, ServerType: " selected-shape ", ServerTypeExplicit: true}, want: "selected-shape"},
		{name: "blank explicit type falls through", cfg: core.Config{Class: "standard", ServerType: " ", ServerTypeExplicit: true}, want: "ccx33"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := (Provider{}).ServerTypeForConfig(test.cfg); got != test.want {
				t.Fatalf("type=%q want=%q", got, test.want)
			}
		})
	}
}

func TestClassSpecs(t *testing.T) {
	want := []core.ClassSpec{
		{Class: "tiny", Type: "ccx13", VCPUs: 2, MemoryGB: 8},
		{Class: "small", Type: "ccx23", VCPUs: 4, MemoryGB: 16},
		{Class: "standard", Type: "ccx33", VCPUs: 8, MemoryGB: 32},
		{Class: "fast", Type: "ccx43", VCPUs: 16, MemoryGB: 64},
		{Class: "large", Type: "ccx53", VCPUs: 32, MemoryGB: 128},
		{Class: "beast", Type: "ccx63", VCPUs: 48, MemoryGB: 192},
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
		"tiny":  {"ccx13", "cpx22", "cx23"},
		"small": {"ccx23", "cpx32", "cx33"},
	}
	for class, want := range tests {
		if got := serverTypeCandidatesForClass(class); !reflect.DeepEqual(got, want) {
			t.Errorf("class=%s candidates=%v want %v", class, got, want)
		}
	}
}

func TestServerShape(t *testing.T) {
	tests := []struct {
		name       string
		serverType string
		wantVCPUs  int
		wantMemory int
	}{
		{name: "known", serverType: "CCX43", wantVCPUs: 16, wantMemory: 64},
		{name: "unknown", serverType: "ccx99"},
		{name: "empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotVCPUs, gotMemory := serverShape(test.serverType)
			if gotVCPUs != test.wantVCPUs || gotMemory != test.wantMemory {
				t.Fatalf("serverShape(%q)=(%d,%d) want (%d,%d)", test.serverType, gotVCPUs, gotMemory, test.wantVCPUs, test.wantMemory)
			}
		})
	}
}
