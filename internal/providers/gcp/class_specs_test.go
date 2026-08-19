package gcp

import (
	"testing"
)

func TestClassProfilesCoverCanonicalClasses(t *testing.T) {
	profiles := (Provider{}).ClassProfiles()
	if len(profiles) != 4 {
		t.Fatalf("ClassProfiles len=%d want 4", len(profiles))
	}
	for _, profile := range profiles {
		if profile.Primary.Type == "" || profile.Fallbacks == nil {
			t.Fatalf("incomplete profile: %#v", profile)
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
