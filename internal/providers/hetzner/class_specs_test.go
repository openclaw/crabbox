package hetzner

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
