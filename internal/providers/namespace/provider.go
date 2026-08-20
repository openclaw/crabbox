package namespace

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

var _ core.ProviderClassProfileProvider = Provider{}

var classProfiles = buildClassProfiles()

func (Provider) Name() string { return namespaceProvider }
func (Provider) Aliases() []string {
	return []string{"namespace", "namespace-devboxes"}
}
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:             namespaceProvider,
		Family:           "namespace",
		Kind:             core.ProviderKindSSHLease,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionMapped,
	}
}

func (Provider) ClassProfiles() []core.ProviderClassProfile {
	return classProfiles
}

func buildClassProfiles() []core.ProviderClassProfile {
	classes := core.CanonicalProviderClasses()
	types := []string{"S", "S", "S", "M", "L", "XL"}
	profiles := make([]core.ProviderClassProfile, 0, len(classes))
	for index, class := range classes {
		profiles = append(profiles, core.ProviderClassProfileFromMachines(
			class, core.TargetLinux, "", core.ProviderClassArchitectureAMD64,
			[]core.ProviderClassMachine{{Type: types[index], Architecture: core.ProviderClassArchitectureAMD64}},
		))
	}
	return profiles
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if strings.TrimSpace(cfg.Namespace.Size) != "" {
		return strings.ToUpper(strings.TrimSpace(cfg.Namespace.Size))
	}
	if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
		return strings.ToUpper(strings.TrimSpace(cfg.ServerType))
	}
	if candidates, matched := core.ProviderClassCandidatesForProfiles(classProfiles, cfg); matched {
		return candidates[0]
	}
	if core.IsCanonicalProviderClass(cfg.Class) {
		return ""
	}
	return namespaceSizeForClass(cfg.Class)
}

func (Provider) ServerTypeOverrideForConfig(cfg core.Config) (string, bool) {
	size := strings.TrimSpace(cfg.Namespace.Size)
	return strings.ToUpper(size), size != ""
}

func (Provider) ServerTypeForClass(class string) string {
	cfg := core.Config{Provider: namespaceProvider, TargetOS: core.TargetLinux, Architecture: core.ArchitectureAMD64, Class: class}
	if candidates, matched := core.ProviderClassCandidatesForProfiles(classProfiles, cfg); matched {
		return candidates[0]
	}
	return namespaceSizeForClass(class)
}

func namespaceSizeForClass(class string) string {
	normalized := strings.ToLower(strings.TrimSpace(class))
	if normalized != class {
		cfg := core.Config{Provider: namespaceProvider, TargetOS: core.TargetLinux, Architecture: core.ArchitectureAMD64, Class: normalized}
		if candidates, matched := core.ProviderClassCandidatesForProfiles(classProfiles, cfg); matched {
			return candidates[0]
		}
	}
	if class == "" {
		return "M"
	}
	return strings.ToUpper(strings.TrimSpace(class))
}
func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterNamespaceProviderFlags(fs, defaults)
}
func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyNamespaceProviderFlags(cfg, fs, values)
}
func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewNamespaceLeaseBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return shared.ConfigureDoctor("namespace-devbox", func() (core.Backend, error) { return p.Configure(cfg, rt) })
}
