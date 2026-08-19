package aws

import (
	"reflect"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestClassSpecs(t *testing.T) {
	want := []core.ClassSpec{
		{Class: "standard", Type: "c7a.8xlarge", VCPUs: 32, MemoryGB: 64},
		{Class: "fast", Type: "c7a.16xlarge", VCPUs: 64, MemoryGB: 128},
		{Class: "large", Type: "c7a.24xlarge", VCPUs: 96, MemoryGB: 192},
		{Class: "beast", Type: "c7a.48xlarge", VCPUs: 192, MemoryGB: 384},
	}
	if got := (Provider{}).ClassSpecs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ClassSpecs()=%#v want %#v", got, want)
	}
}

func TestClassProfilesCoverAWSVariants(t *testing.T) {
	profiles := (Provider{}).ClassProfiles()
	if len(profiles) != 20 {
		t.Fatalf("ClassProfiles len=%d want 20", len(profiles))
	}
	for _, profile := range profiles {
		if profile.Primary.Type == "" || profile.Fallbacks == nil {
			t.Fatalf("incomplete profile: %#v", profile)
		}
		if profile.Target == core.TargetMacOS && profile.Architecture != core.ProviderClassArchitectureMixed {
			t.Fatalf("macOS profile architecture=%q", profile.Architecture)
		}
	}
}

func TestInstanceShape(t *testing.T) {
	tests := []struct {
		name       string
		instance   string
		wantVCPUs  int
		wantMemory int
	}{
		{name: "bare xlarge", instance: "m7i.xlarge", wantVCPUs: 4, wantMemory: 16},
		{name: "numbered xlarge", instance: "r8i.2xlarge", wantVCPUs: 8, wantMemory: 64},
		{name: "small size", instance: "c7a.large", wantVCPUs: 2, wantMemory: 4},
		{name: "unknown family", instance: "z9.2xlarge", wantVCPUs: 8},
		{name: "unparseable size", instance: "c7a.enormous"},
		{name: "missing separator", instance: "c7a"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotVCPUs, gotMemory := instanceShape(test.instance)
			if gotVCPUs != test.wantVCPUs || gotMemory != test.wantMemory {
				t.Fatalf("instanceShape(%q)=(%d,%d) want (%d,%d)", test.instance, gotVCPUs, gotMemory, test.wantVCPUs, test.wantMemory)
			}
		})
	}
}
