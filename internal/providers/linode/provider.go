package linode

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "linode"

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

var _ core.ProviderClassProfileProvider = Provider{}

var classProfiles = core.UniformLinuxAMD64ClassProfiles(core.ProviderClassMachine{Type: defaultType})

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
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

func (Provider) RegisterFlags(*flag.FlagSet, core.Config) any { return core.NoProviderFlags() }
func (Provider) ApplyFlags(*core.Config, *flag.FlagSet, any) error {
	return nil
}

func (p Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && cfg.ServerType != "" {
		return cfg.ServerType
	}
	if cfg.Linode.Type != "" {
		return cfg.Linode.Type
	}
	if candidates, matched := core.ProviderClassCandidatesForProfiles(classProfiles, cfg); matched {
		return candidates[0]
	}
	if core.IsCanonicalProviderClass(cfg.Class) {
		return ""
	}
	return linodeServerTypeForClass(cfg.Class)
}

func (Provider) ServerTypeOverrideForConfig(cfg core.Config) (string, bool) {
	serverType := strings.TrimSpace(cfg.Linode.Type)
	return serverType, serverType != ""
}

func (Provider) ServerTypeForClass(class string) string {
	return linodeServerTypeForClass(class)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewLinodeLeaseBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return newLinodeLeaseBackend(p.Spec(), cfg, rt), nil
}
