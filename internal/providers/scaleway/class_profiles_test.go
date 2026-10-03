package scaleway

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
		{"tiny", "DEV1-S", 2, 2, "", 0, 0},
		{"small", "DEV1-M", 3, 4, "", 0, 0},
		{"standard", "DEV1-L", 4, 8, "PRO2-S", 8, 32},
		{"fast", "PRO2-M", 16, 64, "", 0, 0},
		{"large", "PRO2-L", 32, 128, "", 0, 0},
		{"beast", "GP1-XL", 48, 256, "", 0, 0},
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
			if got := scalewayServerTypeForClass(tc.class); got != tc.machine {
				t.Fatalf("class helper=%q want=%q", got, tc.machine)
			}
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			cfg.Class = tc.class
			core.MarkClassExplicit(&cfg)
			if got := (Provider{}).ServerTypeForConfig(cfg); got != tc.machine {
				t.Fatalf("selected type=%q want=%q", got, tc.machine)
			}
			cfg = (&Backend{cfg: cfg}).cfgForRun()
			if got := serverTypeForConfig(cfg); got != tc.machine {
				t.Fatalf("runtime type=%q want=%q", got, tc.machine)
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
	if got := scalewayServerTypeForClass("unknown"); got != cases[0].machine {
		t.Fatalf("unknown class=%q", got)
	}
}
