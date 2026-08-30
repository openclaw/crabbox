package machine0

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func init() { core.RegisterProvider(Provider{}) }

type Provider struct{}

var machine0ClassProfiles = buildClassProfiles()

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:             providerName,
		Family:           providerName,
		Kind:             core.ProviderKindSSHLease,
		ClassDisposition: core.ProviderClassDispositionMapped,
		SizeSelection:    core.ProviderSizeSelectorType,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features: core.FeatureSet{
			core.FeatureSSH,
			core.FeatureCrabboxSync,
			core.FeatureCleanup,
			core.FeatureDesktop,
			core.FeatureBrowser,
			core.FeatureCode,
			core.FeaturePauseResume,
			core.FeatureCheckpoint,
			core.FeatureFork,
			core.FeatureSnapshot,
		},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return registerFlags(fs, defaults)
}

func (Provider) CreationOnlyFlagNames() []string {
	return []string{
		"machine0-size", "machine0-region", "machine0-image", "machine0-image-version",
		"machine0-desktop-image", "machine0-key",
	}
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return applyFlags(cfg, fs, values)
}

func (p Provider) ApplyConfigDefaults(cfg *core.Config) error {
	applyDefaults(cfg)
	return nil
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	applyDefaults(&cfg)
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.TargetOS != targetLinux {
		return nil, exit(2, "provider=%s supports target=linux only", providerName)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, exit(2, "--tailscale is not supported for provider=%s; use the Machine0 public IP or authenticated HTTPS URL", providerName)
	}
	return newBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return shared.ConfigureDoctor(providerName, func() (core.Backend, error) { return p.Configure(cfg, rt) })
}

func (Provider) ValidateConfig(cfg core.Config) error {
	m := cfg.Machine0
	for name, value := range map[string]string{
		"machine0.cliPath": m.CLIPath,
		"machine0.image":   m.Image,
		"machine0.size":    m.Size,
		"machine0.region":  m.Region,
	} {
		if strings.TrimSpace(value) == "" {
			return exit(2, "%s is required", name)
		}
	}
	switch strings.ToLower(strings.TrimSpace(m.ReleasePolicy)) {
	case "", "destroy", "suspend":
	default:
		return exit(2, "machine0.releasePolicy must be destroy or suspend")
	}
	if m.CreateTimeout <= 0 {
		return exit(2, "machine0.createTimeout must be positive")
	}
	if m.ImageVersion < 0 {
		return exit(2, "machine0.imageVersion must not be negative")
	}
	if m.PollInterval <= 0 {
		return exit(2, "machine0.pollInterval must be positive")
	}
	return nil
}

func (p Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
		return cfg.ServerType
	}
	if size, selected := p.ServerTypeOverrideForConfig(cfg); selected {
		return size
	}
	if core.ClassWasExplicit(cfg) {
		if candidates, matched := core.ProviderClassCandidatesForProfiles(machine0ClassProfiles, cfg); matched {
			return candidates[0]
		}
	}
	return cfg.Machine0.Size
}

func (Provider) ServerTypeOverrideForConfig(cfg core.Config) (string, bool) {
	size := strings.TrimSpace(cfg.Machine0.Size)
	return size, cfg.Machine0.SizeExplicit && size != ""
}

func (Provider) ServerTypeForClass(class string) string {
	for _, profile := range machine0ClassProfiles {
		if profile.Class == class {
			return profile.Primary.Type
		}
	}
	return core.BaseConfig().Machine0.Size
}

func (Provider) ClassProfiles() []core.ProviderClassProfile { return machine0ClassProfiles }

func buildClassProfiles() []core.ProviderClassProfile {
	shapes := []struct {
		size   string
		vcpus  int
		memory int
	}{
		{size: "large", vcpus: 2, memory: 4},
		{size: "xl", vcpus: 4, memory: 8},
		{size: "xxl", vcpus: 8, memory: 16},
		{size: "xxxl", vcpus: 16, memory: 64},
		{size: "4xl", vcpus: 32, memory: 128},
		{size: "5xl", vcpus: 48, memory: 192},
	}
	classes := core.CanonicalProviderClasses()
	profiles := make([]core.ProviderClassProfile, 0, len(classes))
	for index, class := range classes {
		shape := shapes[index]
		profiles = append(profiles, core.ProviderClassProfileFromMachines(
			class, core.TargetLinux, "", core.ProviderClassArchitectureAMD64,
			[]core.ProviderClassMachine{{
				Type: shape.size, Architecture: core.ProviderClassArchitectureAMD64,
				VCPU: &shape.vcpus, Memory: &core.ProviderMemory{Value: float64(shape.memory), Unit: core.ProviderMemoryUnitGB},
			}},
		))
	}
	return profiles
}
