package all

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProductionProviderClassCatalogCompleteness(t *testing.T) {
	mappedProviders := map[string]struct{}{
		"aws": {}, "azure": {}, "cloudflare": {}, "digitalocean": {}, "gcp": {}, "hetzner": {}, "linode": {},
		"namespace-devbox": {}, "namespace-instance": {}, "ovh": {}, "phala": {}, "scaleway": {}, "tencentcloud": {}, "vultr": {},
	}
	counts := map[core.ProviderClassDisposition]int{}
	for _, name := range core.RegisteredProviderNames() {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
		spec := provider.Spec()
		catalog := core.ProviderClassCatalogFor(provider)
		counts[spec.ClassDisposition]++
		source, hasProfiles := provider.(core.ProviderClassProfileProvider)
		_, shouldBeMapped := mappedProviders[name]
		if shouldBeMapped {
			if spec.ClassDisposition != core.ProviderClassDispositionMapped {
				t.Errorf("provider=%s disposition=%q want mapped", name, spec.ClassDisposition)
			}
			if !hasProfiles || len(source.ClassProfiles()) == 0 {
				t.Errorf("provider=%s disposition=mapped requires profiles", name)
			}
			if _, ok := provider.(core.ProviderServerTypeProvider); !ok {
				t.Errorf("provider=%s disposition=mapped requires provider-owned type resolution", name)
			}
			validateProviderClassProfiles(t, provider)
		} else {
			if spec.ClassDisposition != core.ProviderClassDispositionUnmapped {
				t.Errorf("provider=%s disposition=%q want unmapped", name, spec.ClassDisposition)
			}
			if hasProfiles && len(source.ClassProfiles()) > 0 {
				t.Errorf("provider=%s disposition=unmapped must not expose profiles", name)
			}
			if len(catalog.Profiles) != 0 {
				t.Errorf("provider=%s disposition=unmapped catalog profiles=%#v", name, catalog.Profiles)
			}
		}
		if catalog.Profiles == nil {
			t.Errorf("provider=%s catalog profiles is nil", name)
		}
		for _, alias := range provider.Aliases() {
			resolved, err := core.ProviderFor(alias)
			if err != nil {
				t.Errorf("provider=%s alias=%s: %v", name, alias, err)
				continue
			}
			if aliasCatalog := core.ProviderClassCatalogFor(resolved); !reflect.DeepEqual(aliasCatalog, catalog) {
				t.Errorf("provider=%s alias=%s catalog differs: canonical=%#v alias=%#v", name, alias, catalog, aliasCatalog)
			}
		}
	}
	if counts[core.ProviderClassDispositionMapped] != 14 || counts[core.ProviderClassDispositionUnmapped] != 65 || len(counts) != 2 {
		t.Fatalf("class disposition counts=%v want mapped=14 unmapped=65", counts)
	}
}

func TestAWSAndAzureClassProfileVariantCoverage(t *testing.T) {
	want := map[string][]string{
		"aws": {
			"linux//amd64", "linux//arm64", "macos//mixed", "windows/normal/amd64", "windows/wsl2/amd64",
		},
		"azure": {
			"linux//amd64", "linux//arm64", "windows/normal/amd64", "windows/normal/arm64", "windows/wsl2/amd64",
		},
	}
	for name, wantKeys := range want {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatal(err)
		}
		source := provider.(core.ProviderClassProfileProvider)
		keys := map[string]struct{}{}
		for _, profile := range source.ClassProfiles() {
			keys[variantKey(profile)] = struct{}{}
		}
		got := make([]string, 0, len(keys))
		for key := range keys {
			got = append(got, key)
		}
		sort.Strings(got)
		sort.Strings(wantKeys)
		if !reflect.DeepEqual(got, wantKeys) {
			t.Errorf("provider=%s variant keys=%v want %v", name, got, wantKeys)
		}
	}
}

func TestClassProfileCandidatesMatchRuntimeLoops(t *testing.T) {
	for _, name := range []string{"aws", "azure", "gcp", "hetzner"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, profile := range provider.(core.ProviderClassProfileProvider).ClassProfiles() {
			cfg := core.BaseConfig()
			cfg.Provider = name
			cfg.TargetOS = profile.Target
			cfg.WindowsMode = profile.WindowsMode
			cfg.Architecture = string(profile.Architecture)
			if profile.Architecture == core.ProviderClassArchitectureMixed {
				cfg.Architecture = core.ArchitectureAMD64
			}
			cfg.Class = profile.Class
			cfg.ServerType = ""
			cfg.AzureOSDisk = core.AzureOSDiskManaged
			want := profileCandidateTypes(profile)
			var got []string
			switch name {
			case "aws":
				got = core.AWSLaunchCandidates(cfg)
			case "azure":
				got = core.AzureVMSizeCandidatesForConfig(cfg)
			case "gcp":
				got = core.GCPMachineTypeCandidatesForConfig(cfg)
			case "hetzner":
				got = core.HetznerServerTypeCandidatesForConfig(cfg)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("provider=%s selector=%s/%s/%s/%s runtime=%v profiles=%v", name, profile.Class, profile.Target, profile.WindowsMode, profile.Architecture, got, want)
			}
		}
	}
}

func TestMappedProviderProfilePrimaryMatchesProviderResolver(t *testing.T) {
	for _, name := range core.RegisteredProviderNames() {
		provider, err := core.ProviderFor(name)
		if err != nil || provider.Spec().ClassDisposition != core.ProviderClassDispositionMapped {
			continue
		}
		resolver, ok := provider.(core.ProviderServerTypeProvider)
		if !ok {
			continue
		}
		for _, profile := range provider.(core.ProviderClassProfileProvider).ClassProfiles() {
			cfg := core.BaseConfig()
			cfg.Provider = name
			cfg.TargetOS = profile.Target
			cfg.WindowsMode = profile.WindowsMode
			cfg.Architecture = string(profile.Architecture)
			if profile.Architecture == core.ProviderClassArchitectureMixed {
				cfg.Architecture = core.ArchitectureAMD64
			}
			cfg.Class = profile.Class
			cfg.ServerType = ""
			cfg.ServerTypeExplicit = false
			core.MarkClassExplicit(&cfg)
			if got := resolver.ServerTypeForConfig(cfg); got != profile.Primary.Type {
				t.Errorf("provider=%s selector=%s/%s/%s/%s resolver=%q profile=%q", name, profile.Class, profile.Target, profile.WindowsMode, profile.Architecture, got, profile.Primary.Type)
			}
		}
	}
}

func validateProviderClassProfiles(t *testing.T, provider core.Provider) {
	t.Helper()
	profiles := provider.(core.ProviderClassProfileProvider).ClassProfiles()
	selectors := map[string]struct{}{}
	classesByVariant := map[string]map[string]struct{}{}
	for _, profile := range profiles {
		selector := fmt.Sprintf("%s/%s/%s/%s", profile.Class, profile.Target, profile.WindowsMode, profile.Architecture)
		if _, exists := selectors[selector]; exists {
			t.Errorf("duplicate selector %s", selector)
		}
		selectors[selector] = struct{}{}
		if !core.IsCanonicalProviderClass(profile.Class) {
			t.Errorf("selector %s has noncanonical class", selector)
		}
		if !specSupportsTarget(provider.Spec(), profile.Target, profile.WindowsMode) {
			t.Errorf("selector %s is incompatible with provider targets %#v", selector, provider.Spec().Targets)
		}
		if profile.Target != core.TargetWindows && profile.WindowsMode != "" {
			t.Errorf("selector %s has windows mode on non-Windows target", selector)
		}
		if profile.Target == core.TargetWindows && profile.WindowsMode != core.WindowsModeNormal && profile.WindowsMode != core.WindowsModeWSL2 {
			t.Errorf("selector %s has invalid Windows mode", selector)
		}
		if !validProfileArchitecture(profile.Architecture) {
			t.Errorf("selector %s has invalid profile architecture", selector)
		}
		validateClassMachine(t, selector+" primary", profile.Primary, false)
		if profile.Fallbacks == nil {
			t.Errorf("selector %s has nil fallbacks", selector)
		}
		seenTypes := map[string]struct{}{profile.Primary.Type: {}}
		for index, fallback := range profile.Fallbacks {
			validateClassMachine(t, fmt.Sprintf("%s fallback[%d]", selector, index), fallback, false)
			if _, exists := seenTypes[fallback.Type]; exists {
				t.Errorf("selector %s has duplicate machine type %q", selector, fallback.Type)
			}
			seenTypes[fallback.Type] = struct{}{}
		}
		key := variantKey(profile)
		if classesByVariant[key] == nil {
			classesByVariant[key] = map[string]struct{}{}
		}
		classesByVariant[key][profile.Class] = struct{}{}
	}
	for key, classes := range classesByVariant {
		for _, class := range core.CanonicalProviderClasses() {
			if _, ok := classes[class]; !ok {
				t.Errorf("variant=%s missing class=%s", key, class)
			}
		}
	}
}

func validateClassMachine(t *testing.T, label string, machine core.ProviderClassMachine, allowMixed bool) {
	t.Helper()
	if strings.TrimSpace(machine.Type) == "" {
		t.Errorf("%s has blank type", label)
	}
	if machine.Architecture != core.ProviderClassArchitectureAMD64 && machine.Architecture != core.ProviderClassArchitectureARM64 && !(allowMixed && machine.Architecture == core.ProviderClassArchitectureMixed) {
		t.Errorf("%s has invalid concrete architecture %q", label, machine.Architecture)
	}
	if machine.VCPU != nil && *machine.VCPU <= 0 {
		t.Errorf("%s has invalid vcpu=%d", label, *machine.VCPU)
	}
	if machine.Memory != nil {
		if machine.Memory.Value <= 0 {
			t.Errorf("%s has invalid memory=%g", label, machine.Memory.Value)
		}
		switch machine.Memory.Unit {
		case core.ProviderMemoryUnitMB, core.ProviderMemoryUnitMiB, core.ProviderMemoryUnitGB, core.ProviderMemoryUnitGiB:
		default:
			t.Errorf("%s has invalid memory unit=%q", label, machine.Memory.Unit)
		}
	}
}

func variantKey(profile core.ProviderClassProfile) string {
	return fmt.Sprintf("%s/%s/%s", profile.Target, profile.WindowsMode, profile.Architecture)
}

func profileCandidateTypes(profile core.ProviderClassProfile) []string {
	types := []string{profile.Primary.Type}
	for _, fallback := range profile.Fallbacks {
		types = append(types, fallback.Type)
	}
	return types
}

func validProfileArchitecture(architecture core.ProviderClassArchitecture) bool {
	return architecture == core.ProviderClassArchitectureAMD64 || architecture == core.ProviderClassArchitectureARM64 || architecture == core.ProviderClassArchitectureMixed
}

func specSupportsTarget(spec core.ProviderSpec, target, windowsMode string) bool {
	for _, candidate := range spec.Targets {
		if candidate.OS != target {
			continue
		}
		if target != core.TargetWindows || candidate.WindowsMode == windowsMode {
			return true
		}
	}
	return false
}
