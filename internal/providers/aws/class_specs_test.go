package aws

import (
	"reflect"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestClassSpecs(t *testing.T) {
	want := []core.ClassSpec{
		{Class: "tiny", Type: "m7a.large", VCPUs: 2, MemoryGB: 8},
		{Class: "small", Type: "c7a.2xlarge", VCPUs: 8, MemoryGB: 16},
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
	if len(profiles) != 30 {
		t.Fatalf("ClassProfiles len=%d want 30", len(profiles))
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

func TestTinyAndSmallCandidateMappings(t *testing.T) {
	tests := []struct {
		name string
		got  []string
		want []string
	}{
		{name: "linux tiny", got: awsInstanceTypeCandidatesForClass("tiny"), want: []string{"m7a.large", "m7i.large", "c7a.xlarge", "c7i.xlarge", "t3.small"}},
		{name: "linux small", got: awsInstanceTypeCandidatesForClass("small"), want: []string{"c7a.2xlarge", "c7i.2xlarge", "m7a.xlarge", "m7i.xlarge", "c7a.xlarge", "t3.small"}},
		{name: "arm64 tiny", got: awsARM64InstanceTypeCandidatesForClass("tiny"), want: []string{"m7g.large", "c7g.xlarge", "r7g.large", "t4g.small"}},
		{name: "arm64 small", got: awsARM64InstanceTypeCandidatesForClass("small"), want: []string{"c7g.2xlarge", "m7g.xlarge", "r7g.large", "c7g.xlarge", "t4g.small"}},
		{name: "windows tiny", got: awsWindowsInstanceTypeCandidatesForClass("tiny"), want: []string{"m7a.large", "m7i.large", "t3.large"}},
		{name: "windows small", got: awsWindowsInstanceTypeCandidatesForClass("small"), want: []string{"c7a.2xlarge", "c7i.2xlarge", "m7a.xlarge", "m7i.xlarge", "t3.xlarge", "t3.large"}},
		{name: "wsl2 tiny", got: awsWSL2InstanceTypeCandidatesForClass("tiny"), want: []string{"m8i.large", "m8i-flex.large", "c8i.xlarge", "r8i.large"}},
		{name: "wsl2 small", got: awsWSL2InstanceTypeCandidatesForClass("small"), want: []string{"c8i.2xlarge", "m8i.xlarge", "m8i-flex.xlarge", "r8i.large", "c8i.xlarge", "m8i.large"}},
	}
	for _, test := range tests {
		if !reflect.DeepEqual(test.got, test.want) {
			t.Errorf("%s=%v want %v", test.name, test.got, test.want)
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
