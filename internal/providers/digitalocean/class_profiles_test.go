package digitalocean

import (
	"reflect"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestClassProfilesSelectRealSizes(t *testing.T) {
	cases := []struct {
		class, machine string
		vcpu           int
		memory         float64
		fallback       string
		fallbackVCPU   int
		fallbackMemory float64
	}{
		{"tiny", "s-1vcpu-1gb", 1, 1, "", 0, 0},
		{"small", "s-2vcpu-4gb", 2, 4, "", 0, 0},
		{"standard", "s-4vcpu-8gb", 4, 8, "", 0, 0},
		{"fast", "s-8vcpu-16gb", 8, 16, "c-8", 8, 16},
		{"large", "g-16vcpu-64gb", 16, 64, "c-16", 16, 32},
		{"beast", "g-32vcpu-128gb", 32, 128, "c-32", 32, 64},
	}
	profiles := (Provider{}).ClassProfiles()
	if len(profiles) != len(cases) {
		t.Fatalf("profiles=%d want=%d", len(profiles), len(cases))
	}
	for index, tc := range cases {
		t.Run(tc.class, func(t *testing.T) {
			profile := profiles[index]
			if profile.Class != tc.class || profile.Target != core.TargetLinux || profile.Architecture != core.ProviderClassArchitectureAMD64 {
				t.Fatalf("profile selector/order=%#v", profile)
			}
			checkMachine := func(machine core.ProviderClassMachine, wantType string, wantVCPU int, wantMemory float64) {
				t.Helper()
				if machine.Type != wantType || machine.Architecture != core.ProviderClassArchitectureAMD64 || machine.VCPU == nil || *machine.VCPU != wantVCPU || machine.Memory == nil || *machine.Memory != (core.ProviderMemory{Value: wantMemory, Unit: core.ProviderMemoryUnitGiB}) {
					t.Fatalf("machine=%#v want=%s/%d vCPU/%g GiB", machine, wantType, wantVCPU, wantMemory)
				}
			}
			checkMachine(profile.Primary, tc.machine, tc.vcpu, tc.memory)
			if tc.fallback == "" {
				if profile.Fallbacks == nil || len(profile.Fallbacks) != 0 {
					t.Fatalf("fallbacks=%#v", profile.Fallbacks)
				}
			} else {
				if len(profile.Fallbacks) != 1 {
					t.Fatalf("fallbacks=%#v", profile.Fallbacks)
				}
				checkMachine(profile.Fallbacks[0], tc.fallback, tc.fallbackVCPU, tc.fallbackMemory)
			}
			if got := digitalOceanServerTypeForClass(tc.class); got != tc.machine {
				t.Fatalf("class helper=%q want=%q", got, tc.machine)
			}
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			cfg.Class = tc.class
			core.MarkClassExplicit(&cfg)
			if got := (Provider{}).ServerTypeForConfig(cfg); got != tc.machine {
				t.Fatalf("selected type=%q want=%q", got, tc.machine)
			}
			applyDigitalOceanDefaults(&cfg)
			if cfg.ServerType != tc.machine {
				t.Fatalf("runtime type=%q want=%q", cfg.ServerType, tc.machine)
			}
			cfg.ServerType, cfg.ServerTypeExplicit = "custom-type", true
			if got := (Provider{}).ServerTypeForConfig(cfg); got != "custom-type" {
				t.Fatalf("explicit type=%q", got)
			}
		})
	}
	if !reflect.DeepEqual(profiles, (Provider{}).ClassProfiles()) {
		t.Fatal("class profiles changed between calls")
	}
	if got := digitalOceanServerTypeForClass("unknown"); got != cases[0].machine {
		t.Fatalf("unknown class=%q", got)
	}
}
