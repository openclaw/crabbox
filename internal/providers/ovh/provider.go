package ovh

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "ovh"

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      providerName,
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureTailscale},
		Coordinator: core.CoordinatorNever,
	}
}

type flagValues struct {
	Endpoint  *string
	ProjectID *string
	Region    *string
	Image     *string
	Flavor    *string
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		Endpoint:  fs.String("ovh-endpoint", defaults.OVH.Endpoint, "OVHcloud API endpoint"),
		ProjectID: fs.String("ovh-project-id", defaults.OVH.ProjectID, "OVHcloud Public Cloud project ID"),
		Region:    fs.String("ovh-region", defaults.OVH.Region, "OVHcloud Public Cloud region"),
		Image:     fs.String("ovh-image", defaults.OVH.Image, "OVHcloud Public Cloud image name or ID"),
		Flavor:    fs.String("ovh-flavor", defaults.OVH.Flavor, "OVHcloud Public Cloud flavor name or ID"),
	}
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "ovh-endpoint") {
		cfg.OVH.Endpoint = *v.Endpoint
	}
	if core.FlagWasSet(fs, "ovh-project-id") {
		cfg.OVH.ProjectID = *v.ProjectID
	}
	if core.FlagWasSet(fs, "ovh-region") {
		cfg.OVH.Region = *v.Region
	}
	if core.FlagWasSet(fs, "ovh-image") {
		cfg.OVH.Image = *v.Image
		core.SetOVHImageExplicit(cfg)
	}
	if core.FlagWasSet(fs, "ovh-flavor") {
		cfg.OVH.Flavor = *v.Flavor
	}
	return nil
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && cfg.ServerType != "" {
		return cfg.ServerType
	}
	if cfg.OVH.Flavor != "" {
		return cfg.OVH.Flavor
	}
	return ovhServerTypeForClass(cfg.Class)
}

func (Provider) ServerTypeForClass(class string) string {
	return ovhServerTypeForClass(class)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return NewBackend(p.Spec(), cfg, rt), nil
}

func ovhServerTypeForClass(class string) string {
	switch class {
	case "standard", "fast", "large", "beast":
		return "b3-8"
	default:
		return "b3-8"
	}
}
