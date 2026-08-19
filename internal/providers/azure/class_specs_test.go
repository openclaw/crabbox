package azure

import (
	"reflect"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestClassSpecs(t *testing.T) {
	want := []core.ClassSpec{
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
	if len(profiles) != 20 {
		t.Fatalf("ClassProfiles len=%d want 20", len(profiles))
	}
	for _, profile := range profiles {
		if profile.Primary.Type == "" || profile.Fallbacks == nil {
			t.Fatalf("incomplete profile: %#v", profile)
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
