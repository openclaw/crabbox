package tencentcloud

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "tencentcloud"

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

var classProfiles = buildClassProfiles()

var _ core.ProviderClassProfileProvider = Provider{}

func (Provider) Name() string { return providerName }
func (Provider) Aliases() []string {
	return []string{"tencent", "tencent-cvm", "cvm"}
}

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

func buildClassProfiles() []core.ProviderClassProfile {
	classes := core.CanonicalProviderClasses()
	types := []string{defaultType, defaultType, defaultType, "SA5.LARGE8", "SA5.2XLARGE16", "SA5.8XLARGE64"}
	profiles := make([]core.ProviderClassProfile, 0, len(classes))
	for index, class := range classes {
		profiles = append(profiles, core.ProviderClassProfileFromMachines(
			class, core.TargetLinux, "", core.ProviderClassArchitectureAMD64,
			[]core.ProviderClassMachine{{Type: types[index], Architecture: core.ProviderClassArchitectureAMD64}},
		))
	}
	return profiles
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterTencentCloudConfigFlags(fs, defaults.TencentCloud)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.TencentCloudConfigFlagValues)
	if !ok {
		return nil
	}
	core.MarkTencentCloudConfigApplied(cfg, v.Apply(&cfg.TencentCloud, fs))
	return nil
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	return serverTypeForConfig(cfg)
}

func (Provider) ServerTypeOverrideForConfig(cfg core.Config) (string, bool) {
	serverType := strings.TrimSpace(cfg.TencentCloud.Type)
	return serverType, core.TencentCloudTypeWasExplicit(cfg) && serverType != ""
}

func (Provider) ServerTypeForClass(class string) string {
	return serverTypeForClass(class)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend := NewBackend(p.Spec(), cfg, rt)
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "tencentcloud doctor backend unavailable")
	}
	return doctor, nil
}
