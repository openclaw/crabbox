package cli

import (
	"math"
	"sort"
	"strings"
)

type ProviderClassDisposition string

const (
	ProviderClassDispositionMapped   ProviderClassDisposition = "mapped"
	ProviderClassDispositionUnmapped ProviderClassDisposition = "unmapped"
)

type ProviderClassArchitecture string

const (
	ProviderClassArchitectureAMD64 ProviderClassArchitecture = "amd64"
	ProviderClassArchitectureARM64 ProviderClassArchitecture = "arm64"
	ProviderClassArchitectureMixed ProviderClassArchitecture = "mixed"
)

type ProviderMemoryUnit string

const (
	ProviderMemoryUnitMB  ProviderMemoryUnit = "MB"
	ProviderMemoryUnitMiB ProviderMemoryUnit = "MiB"
	ProviderMemoryUnitGB  ProviderMemoryUnit = "GB"
	ProviderMemoryUnitGiB ProviderMemoryUnit = "GiB"
)

type ProviderMemory struct {
	Value float64            `json:"value"`
	Unit  ProviderMemoryUnit `json:"unit"`
}

type ProviderClassMachine struct {
	Type         string                    `json:"type"`
	Architecture ProviderClassArchitecture `json:"architecture"`
	VCPU         *int                      `json:"vcpu"`
	Memory       *ProviderMemory           `json:"memory"`
}

type ProviderClassProfile struct {
	Class        string                    `json:"class"`
	Target       string                    `json:"target"`
	WindowsMode  string                    `json:"windowsMode,omitempty"`
	Architecture ProviderClassArchitecture `json:"architecture"`
	Primary      ProviderClassMachine      `json:"primary"`
	Fallbacks    []ProviderClassMachine    `json:"fallbacks"`
}

type ProviderClassCatalog struct {
	Disposition ProviderClassDisposition `json:"disposition"`
	Profiles    []ProviderClassProfile   `json:"profiles"`
}

type ProviderClassProfileProvider interface {
	ClassProfiles() []ProviderClassProfile
}

var canonicalProviderClasses = [...]string{"tiny", "small", "standard", "fast", "large", "beast"}

func CanonicalProviderClasses() []string {
	return append([]string(nil), canonicalProviderClasses[:]...)
}

func IsCanonicalProviderClass(class string) bool {
	for _, canonical := range canonicalProviderClasses {
		if class == canonical {
			return true
		}
	}
	return false
}

func concreteStoredServerType(cfg Config) string {
	if cfg.ServerType == cfg.Class || IsCanonicalProviderClass(cfg.ServerType) {
		return ""
	}
	if strings.TrimSpace(cfg.ServerType) == "" {
		return ""
	}
	return cfg.ServerType
}

func appendUniqueExactStrings(values []string, extra ...string) []string {
	out := make([]string, 0, len(values)+len(extra))
	seen := make(map[string]bool, len(values)+len(extra))
	appendValue := func(value string) {
		if strings.TrimSpace(value) == "" || seen[value] {
			return
		}
		seen[value] = true
		out = append(out, value)
	}
	for _, value := range values {
		appendValue(value)
	}
	for _, value := range extra {
		appendValue(value)
	}
	return out
}

func ProviderClassProfileFromMachines(class, target, windowsMode string, architecture ProviderClassArchitecture, machines []ProviderClassMachine) ProviderClassProfile {
	profile := ProviderClassProfile{
		Class: class, Target: target, WindowsMode: windowsMode, Architecture: architecture,
		Fallbacks: []ProviderClassMachine{},
	}
	if len(machines) == 0 {
		return profile
	}
	profile.Primary = machines[0]
	profile.Fallbacks = append(profile.Fallbacks, machines[1:]...)
	return profile
}

func UniformLinuxAMD64ClassProfiles(machine ProviderClassMachine) []ProviderClassProfile {
	machine.Architecture = ProviderClassArchitectureAMD64
	classes := CanonicalProviderClasses()
	profiles := make([]ProviderClassProfile, 0, len(classes))
	for _, class := range classes {
		profiles = append(profiles, ProviderClassProfileFromMachines(
			class, TargetLinux, "", ProviderClassArchitectureAMD64, []ProviderClassMachine{machine},
		))
	}
	return profiles
}

// ProviderClassSpecsFromProfiles projects the default Linux/amd64 primary
// machines into the compatibility class summary used by provider discovery.
func ProviderClassSpecsFromProfiles(profiles []ProviderClassProfile) []ClassSpec {
	specs := make([]ClassSpec, 0, len(canonicalProviderClasses))
	for _, class := range canonicalProviderClasses {
		for _, profile := range profiles {
			if profile.Class != class ||
				normalizeTargetOS(profile.Target) != targetLinux ||
				profile.Architecture != ProviderClassArchitectureAMD64 {
				continue
			}
			spec := ClassSpec{Class: class, Type: profile.Primary.Type}
			if profile.Primary.VCPU != nil {
				spec.VCPUs = *profile.Primary.VCPU
			}
			if memory := profile.Primary.Memory; memory != nil &&
				(memory.Unit == ProviderMemoryUnitGB || memory.Unit == ProviderMemoryUnitGiB) &&
				memory.Value > 0 && memory.Value == math.Trunc(memory.Value) {
				spec.MemoryGB = int(memory.Value)
			}
			specs = append(specs, spec)
			break
		}
	}
	return specs
}

func providerClassCatalogFor(provider Provider) ProviderClassCatalog {
	catalog := ProviderClassCatalog{
		Disposition: provider.Spec().ClassDisposition,
		Profiles:    []ProviderClassProfile{},
	}
	if source, ok := provider.(ProviderClassProfileProvider); ok {
		catalog.Profiles = cloneProviderClassProfiles(source.ClassProfiles())
	}
	sort.SliceStable(catalog.Profiles, func(i, j int) bool {
		left, right := catalog.Profiles[i], catalog.Profiles[j]
		if classOrder(left.Class) != classOrder(right.Class) {
			return classOrder(left.Class) < classOrder(right.Class)
		}
		if left.Target != right.Target {
			return left.Target < right.Target
		}
		if left.WindowsMode != right.WindowsMode {
			return left.WindowsMode < right.WindowsMode
		}
		return left.Architecture < right.Architecture
	})
	return catalog
}

func providerClassCandidatesForConfig(cfg Config) ([]string, bool) {
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return nil, false
	}
	source, ok := provider.(ProviderClassProfileProvider)
	if !ok {
		return nil, false
	}
	return ProviderClassCandidatesForProfiles(source.ClassProfiles(), cfg)
}

func ProviderClassCandidatesForProfiles(profiles []ProviderClassProfile, cfg Config) ([]string, bool) {
	class := cfg.Class
	target := normalizeTargetOS(cfg.TargetOS)
	windowsMode := ""
	if target == targetWindows {
		windowsMode = normalizeWindowsMode(cfg.WindowsMode)
	}
	architectureValue := effectiveArchitectureForConfig(cfg)
	if normalized, err := normalizeArchitecture(architectureValue); err == nil {
		architectureValue = normalized
	}
	architecture := ProviderClassArchitecture(architectureValue)
	mixedIndex := -1
	for index := range profiles {
		profile := &profiles[index]
		if profile.Class != class ||
			normalizeTargetOS(profile.Target) != target ||
			normalizeProfileWindowsMode(profile.Target, profile.WindowsMode) != windowsMode {
			continue
		}
		if profile.Architecture == architecture {
			return providerClassCandidateTypes(*profile), true
		}
		if profile.Architecture == ProviderClassArchitectureMixed {
			mixedIndex = index
		}
	}
	if mixedIndex >= 0 {
		return providerClassCandidateTypes(profiles[mixedIndex]), true
	}
	return nil, false
}

func providerClassCandidateTypes(profile ProviderClassProfile) []string {
	candidates := make([]string, 0, 1+len(profile.Fallbacks))
	candidates = append(candidates, profile.Primary.Type)
	for _, fallback := range profile.Fallbacks {
		candidates = append(candidates, fallback.Type)
	}
	return candidates
}

func validateProviderClassSelector(provider Provider, cfg Config) error {
	if provider.Spec().ClassDisposition != ProviderClassDispositionMapped ||
		(cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "") ||
		!IsCanonicalProviderClass(cfg.Class) {
		return nil
	}
	source, ok := provider.(ProviderClassProfileProvider)
	if !ok {
		return exit(2, "provider=%s declares mapped classes without class profiles", provider.Name())
	}
	if _, matched := ProviderClassCandidatesForProfiles(source.ClassProfiles(), cfg); matched {
		return nil
	}
	// Provider-native or stored exact selection outranks class-profile validation;
	// inherited provider defaults and canonical class placeholders do not.
	if storedType := concreteStoredServerType(cfg); storedType != "" {
		return nil
	}
	if override, ok := provider.(ProviderServerTypeOverrideProvider); ok {
		if resolvedType, selected := override.ServerTypeOverrideForConfig(cfg); selected && strings.TrimSpace(resolvedType) != "" {
			return nil
		}
	}
	target := normalizeTargetOS(cfg.TargetOS)
	windowsMode := ""
	if target == targetWindows {
		windowsMode = normalizeWindowsMode(cfg.WindowsMode)
	}
	architecture := effectiveArchitectureForConfig(cfg)
	if normalized, err := normalizeArchitecture(architecture); err == nil {
		architecture = normalized
	}
	return exit(2, "provider=%s has no class profile for class=%s target=%s windowsMode=%s architecture=%s", provider.Name(), cfg.Class, target, blank(windowsMode, "-"), architecture)
}

func normalizeProfileWindowsMode(target, mode string) string {
	if normalizeTargetOS(target) != targetWindows {
		return ""
	}
	return normalizeWindowsMode(mode)
}

func classOrder(class string) int {
	for index, canonical := range canonicalProviderClasses {
		if class == canonical {
			return index
		}
	}
	return len(canonicalProviderClasses)
}

func cloneProviderClassProfiles(profiles []ProviderClassProfile) []ProviderClassProfile {
	if profiles == nil {
		return []ProviderClassProfile{}
	}
	cloned := make([]ProviderClassProfile, len(profiles))
	for index, profile := range profiles {
		cloned[index] = cloneProviderClassProfile(profile)
	}
	return cloned
}

func cloneProviderClassProfile(profile ProviderClassProfile) ProviderClassProfile {
	profile.Primary = cloneProviderClassMachine(profile.Primary)
	if profile.Fallbacks == nil {
		profile.Fallbacks = []ProviderClassMachine{}
	} else {
		fallbacks := make([]ProviderClassMachine, len(profile.Fallbacks))
		for index, machine := range profile.Fallbacks {
			fallbacks[index] = cloneProviderClassMachine(machine)
		}
		profile.Fallbacks = fallbacks
	}
	return profile
}

func cloneProviderClassMachine(machine ProviderClassMachine) ProviderClassMachine {
	if machine.VCPU != nil {
		value := *machine.VCPU
		machine.VCPU = &value
	}
	if machine.Memory != nil {
		value := *machine.Memory
		machine.Memory = &value
	}
	return machine
}
