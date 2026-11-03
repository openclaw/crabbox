package asciibox

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

var classProfiles = buildClassProfiles()

func buildClassProfiles() []core.ProviderClassProfile {
	// Boat has four sizes; adjacent Crabbox classes share its end tiers.
	types := []string{"small", "small", "default", "large", "xlarge", "xlarge"}
	cpus := []int{2, 2, 4, 8, 16, 16}
	profiles := make([]core.ProviderClassProfile, 0, len(types))
	for i, class := range core.CanonicalProviderClasses() {
		cpu := cpus[i]
		profiles = append(profiles, core.ProviderClassProfileFromMachines(class, core.TargetLinux, "", core.ProviderClassArchitectureAMD64, []core.ProviderClassMachine{{
			Type: types[i], Architecture: core.ProviderClassArchitectureAMD64,
			VCPU: &cpu, Memory: &core.ProviderMemory{Value: float64(cpu * 2), Unit: core.ProviderMemoryUnitGB},
		}}))
	}
	return profiles
}

func (Provider) ClassProfiles() []core.ProviderClassProfile { return classProfiles }

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
		return strings.TrimSpace(cfg.ServerType)
	}
	if core.ClassWasExplicit(cfg) {
		return core.ProviderClassPrimaryTypeForProfiles(classProfiles, cfg, cfg.Class)
	}
	// Existing callers without sizing selected Boat's native default.
	return "default"
}

func validateBoxType(kind string) error {
	switch kind {
	case "small", "default", "large", "xlarge":
		return nil
	default:
		return core.Exit(2, "provider=ascii-box requires --type small, default, large, or xlarge; got %q", kind)
	}
}
