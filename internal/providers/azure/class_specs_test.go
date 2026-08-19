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

func TestVMShape(t *testing.T) {
	tests := []struct {
		name       string
		vmSize     string
		wantVCPUs  int
		wantMemory int
	}{
		{name: "Dv6", vmSize: "Standard_D32ads_v6", wantVCPUs: 32, wantMemory: 128},
		{name: "not Dv6", vmSize: "Standard_D32ads_v5"},
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
