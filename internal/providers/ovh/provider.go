package ovh

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "ovh"

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

var _ core.ProviderClassProfileProvider = Provider{}

var classProfiles = core.UniformLinuxAMD64ClassProfiles(core.ProviderClassMachine{Type: "b3-8"})

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Authentication:   core.DirectProviderAuthentication(core.ProviderAuthenticationAPICredentials),
		Name:             providerName,
		Family:           providerName,
		Kind:             core.ProviderKindSSHLease,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureTailscale},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionMapped,
	}
}

func (Provider) ClassProfiles() []core.ProviderClassProfile {
	return classProfiles
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterOVHConfigFlags(fs, defaults.OVH)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.OVHConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.OVH, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if applied.Image {
		core.SetOVHImageExplicit(cfg)
	}
	return err
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && cfg.ServerType != "" {
		return cfg.ServerType
	}
	if cfg.OVH.Flavor != "" {
		return cfg.OVH.Flavor
	}
	if candidates, matched := core.ProviderClassCandidatesForProfiles(classProfiles, cfg); matched {
		return candidates[0]
	}
	if core.IsCanonicalProviderClass(cfg.Class) {
		return ""
	}
	return ovhServerTypeForClass(cfg.Class)
}

func (Provider) ServerTypeOverrideForConfig(cfg core.Config) (string, bool) {
	flavor := strings.TrimSpace(cfg.OVH.Flavor)
	return flavor, flavor != ""
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewBackend(p.Spec(), cfg, rt), nil
}

func ovhServerTypeForClass(class string) string {
	for _, profile := range classProfiles {
		if profile.Class == class {
			return profile.Primary.Type
		}
	}
	return "b3-8"
}
