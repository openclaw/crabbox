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

func (Provider) NormalizeConfigForShow(cfg core.Config) core.Config {
	core.ApplyConfigShowSSHDefaults(&cfg, "root")
	return cfg
}

var _ core.ProviderClassProfileProvider = Provider{}

var classProfiles = core.UniformLinuxAMD64ClassProfiles(core.ProviderClassMachine{Type: defaultType})

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Authentication:   core.DirectProviderAuthentication(core.ProviderAuthenticationAPIToken),
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
	return core.ProviderClassPrimaryTypeForProfiles(classProfiles, cfg, linodeServerTypeForClass(cfg.Class))
}

func (Provider) ServerTypeOverrideForConfig(cfg core.Config) (string, bool) {
	serverType := strings.TrimSpace(cfg.Linode.Type)
	return serverType, serverType != ""
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewLinodeLeaseBackend(p.Spec(), cfg, rt), nil
}

func (Provider) ApplyConfigDefaults(cfg *core.Config) error {
	applyNativeDefaults(&cfg.Linode)
	if core.OSImageWasExplicit(*cfg) && !core.LinodeImageWasExplicit(*cfg) {
		if cfg.OSImage == "ubuntu:24.04" {
			cfg.Linode.Image = "linode/ubuntu24.04"
		} else {
			// Leave unsupported intent unresolved until acquisition validation.
			cfg.Linode.Image = ""
		}
	}
	if cfg.Linode.Type == "" {
		cfg.Linode.Type = core.LinodeConfiguredTypeDefault
	}
	base := core.BaseConfig()
	core.ApplyLinuxConnectionDefaults(cfg, base.SSHUser, base.SSHPort)
	return nil
}

func applyNativeDefaults(cfg *core.LinodeConfig) {
	if cfg.Region == "" {
		cfg.Region = core.LinodeConfiguredRegionDefault
	}
	if cfg.Image == "" {
		cfg.Image = core.LinodeImageFallback
	}
}
