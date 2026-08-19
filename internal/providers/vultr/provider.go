package vultr

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "vultr"

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

var _ core.ProviderClassProfileProvider = Provider{}

var classProfiles = core.UniformLinuxAMD64ClassProfiles(core.ProviderClassMachine{Type: "vc2-1c-1gb"})

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:             providerName,
		Family:           providerName,
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

func (Provider) RegisterFlags(*flag.FlagSet, core.Config) any { return core.NoProviderFlags() }

func (Provider) ApplyFlags(*core.Config, *flag.FlagSet, any) error {
	return nil
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && cfg.ServerType != "" {
		return cfg.ServerType
	}
	if candidates, matched := core.ProviderClassCandidatesForProfiles(classProfiles, cfg); matched {
		return candidates[0]
	}
	if core.IsCanonicalProviderClass(cfg.Class) {
		return ""
	}
	return vultrServerTypeForClass(cfg.Class)
}

func (Provider) ServerTypeForClass(class string) string {
	return vultrServerTypeForClass(class)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "vultr doctor backend unavailable")
	}
	return doctor, nil
}

func vultrServerTypeForClass(class string) string {
	for _, profile := range classProfiles {
		if profile.Class == class {
			return profile.Primary.Type
		}
	}
	return "vc2-1c-1gb"
}
