package namespaceinstance

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
		wantMemory int
	}{
		{name: "known", machine: "8x16", wantVCPUs: 8, wantMemory: 16},
		{name: "unparseable CPU", machine: "manyx16"},
		{name: "unparseable memory", machine: "8xmany"},
		{name: "extra separator", machine: "8x16x32"},
		{name: "zero", machine: "0x16"},
		{name: "missing separator", machine: "8"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotVCPUs, gotMemory := machineShape(test.machine)
			if gotVCPUs != test.wantVCPUs || gotMemory != test.wantMemory {
				t.Fatalf("machineShape(%q)=(%d,%d) want (%d,%d)", test.machine, gotVCPUs, gotMemory, test.wantVCPUs, test.wantMemory)
			}
		})
	}
}
