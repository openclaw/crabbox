package azure

import (
	"reflect"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestClassSpecs(t *testing.T) {
	want := []core.ClassSpec{
		{Class: "tiny", Type: "Standard_D2ads_v6", VCPUs: 2, MemoryGB: 8},
		{Class: "small", Type: "Standard_D8ads_v6", VCPUs: 8, MemoryGB: 32},
		{Class: "standard", Type: "Standard_D32ads_v6", VCPUs: 32, MemoryGB: 128},
		{Class: "fast", Type: "Standard_D64ads_v6", VCPUs: 64, MemoryGB: 256},
		{Class: "large", Type: "Standard_D96ads_v6", VCPUs: 96, MemoryGB: 384},
		{Class: "beast", Type: "Standard_D192ds_v6", VCPUs: 192, MemoryGB: 768},
	}
	if got := (Provider{}).ClassSpecs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ClassSpecs()=%#v want %#v", got, want)
	}
}

func TestClassProfilesCoverAzureVariants(t *testing.T) {
	profiles := (Provider{}).ClassProfiles()
	if len(profiles) != 30 {
		t.Fatalf("ClassProfiles len=%d want 30", len(profiles))
	}
	for _, profile := range profiles {
		if profile.Primary.Type == "" || profile.Fallbacks == nil {
			t.Fatalf("incomplete profile: %#v", profile)
		}
	}
}

func TestTinyAndSmallCandidateMappings(t *testing.T) {
	tests := []struct {
		name string
		got  []string
		want []string
	}{
		{name: "linux tiny", got: azureVMSizeCandidatesForClass("tiny"), want: []string{"Standard_D2ads_v6", "Standard_D2ds_v6", "Standard_D2ads_v5", "Standard_D2ds_v5", "Standard_F2s_v2"}},
		{name: "linux small", got: azureVMSizeCandidatesForClass("small"), want: []string{"Standard_D8ads_v6", "Standard_D8ds_v6", "Standard_F8s_v2", "Standard_D8ads_v5", "Standard_D8ds_v5", "Standard_D4ads_v6", "Standard_D4ds_v6", "Standard_F4s_v2"}},
		{name: "arm64 tiny", got: azureARM64VMSizeCandidatesForClass("tiny"), want: []string{"Standard_D2pds_v6", "Standard_D2ps_v6"}},
		{name: "arm64 small", got: azureARM64VMSizeCandidatesForClass("small"), want: []string{"Standard_D8pds_v6", "Standard_D8ps_v6", "Standard_D4pds_v6", "Standard_D4ps_v6"}},
		{name: "windows tiny", got: azureWindowsVMSizeCandidatesForClass("tiny"), want: []string{"Standard_D2ads_v6", "Standard_D2ds_v6", "Standard_D2ads_v5", "Standard_D2ds_v5", "Standard_D2as_v6"}},
		{name: "windows small", got: azureWindowsVMSizeCandidatesForClass("small"), want: []string{"Standard_D8ads_v6", "Standard_D8ds_v6", "Standard_D8ads_v5", "Standard_D8ds_v5", "Standard_D8as_v6"}},
	}
	for _, test := range tests {
		if !reflect.DeepEqual(test.got, test.want) {
			t.Errorf("%s=%v want %v", test.name, test.got, test.want)
		}
	}
}

func TestVMShape(t *testing.T) {
	tests := []struct {
		name       string
		vmSize     string
		wantVCPUs  int
		wantMemory int
	}{
		{name: "Dv6", vmSize: "Standard_D32ads_v6", wantVCPUs: 32, wantMemory: 128},
		{name: "Dv5", vmSize: "Standard_D32ads_v5", wantVCPUs: 32, wantMemory: 128},
		{name: "non-numeric D family", vmSize: "Standard_DC32ads_v6"},
		{name: "unparseable", vmSize: "not-a-vm-size"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotVCPUs, gotMemory := vmShape(test.vmSize)
			if gotVCPUs != test.wantVCPUs || gotMemory != test.wantMemory {
				t.Fatalf("vmShape(%q)=(%d,%d) want (%d,%d)", test.vmSize, gotVCPUs, gotMemory, test.wantVCPUs, test.wantMemory)
			}
		})
	}
}

func TestServerTypeForConfigDoesNotReuseClassPlaceholder(t *testing.T) {
	cfg := core.Config{
		Provider: "azure", TargetOS: core.TargetLinux, Architecture: core.ArchitectureAMD64,
		Class: "standard", ServerType: "standard",
	}
	if got := (Provider{}).ServerTypeForConfig(cfg); got != "Standard_D32ads_v6" {
		t.Fatalf("server type=%q want profile primary", got)
	}
}
