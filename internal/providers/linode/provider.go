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

var classProfiles = buildClassProfiles()

func buildClassProfiles() []core.ProviderClassProfile {
	// Shapes follow https://api.linode.com/v4/linode/types (memory converted from MiB).
	machine := func(serverType string, vcpu int, memoryGiB float64) core.ProviderClassMachine {
		return core.ProviderClassMachine{
			Type: serverType, Architecture: core.ProviderClassArchitectureAMD64, VCPU: &vcpu,
			Memory: &core.ProviderMemory{Value: memoryGiB, Unit: core.ProviderMemoryUnitGiB},
		}
	}
	machines := map[string][]core.ProviderClassMachine{
		"tiny":     {machine("g6-standard-1", 1, 2)},
		"small":    {machine("g6-standard-2", 2, 4)},
		"standard": {machine("g6-standard-4", 4, 8)},
		"fast":     {machine("g6-standard-6", 6, 16)},
		"large":    {machine("g6-standard-8", 8, 32)},
		"beast":    {machine("g6-standard-16", 16, 64)},
	}
	profiles := make([]core.ProviderClassProfile, 0, len(core.CanonicalProviderClasses()))
	for _, class := range core.CanonicalProviderClasses() {
		profiles = append(profiles, core.ProviderClassProfileFromMachines(
			class, core.TargetLinux, "", core.ProviderClassArchitectureAMD64, machines[class],
		))
	}
	return profiles
}

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
	if cfg.Linode.Type != "" && linodeTypeOverridesClass(cfg) {
		return cfg.Linode.Type
	}
	return core.ProviderClassPrimaryTypeForProfiles(classProfiles, cfg, linodeServerTypeForClass(cfg.Class))
}

func (Provider) ServerTypeOverrideForConfig(cfg core.Config) (string, bool) {
	serverType := strings.TrimSpace(cfg.Linode.Type)
	return serverType, serverType != "" && linodeTypeOverridesClass(cfg)
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
