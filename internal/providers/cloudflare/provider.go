package cloudflare

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

var classProfiles = core.UniformLinuxAMD64ClassProfiles(core.ProviderClassMachine{Type: "standard-4"})

func (Provider) Name() string { return providerName }

func (Provider) Aliases() []string {
	return []string{providerAlias}
}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		SyncGuardrailFullCandidate: true,
		Name:                       providerName,
		Family:                     "cloudflare",
		Kind:                       core.ProviderKindDelegatedRun,
		Targets:                    []core.TargetSpec{{OS: core.TargetLinux}},
		Features:                   core.FeatureSet{core.FeatureArchiveSync, core.FeatureCleanup, core.FeatureRunSession},
		Coordinator:                core.CoordinatorNever,
		ClassDisposition:           core.ProviderClassDispositionMapped,
	}
}

func (Provider) ClassProfiles() []core.ProviderClassProfile {
	return classProfiles
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
		return strings.TrimSpace(cfg.ServerType)
	}
	if candidates, matched := core.ProviderClassCandidatesForProfiles(classProfiles, cfg); matched {
		return candidates[0]
	}
	if core.IsCanonicalProviderClass(cfg.Class) {
		return ""
	}
	return cloudflareTypeForClass(cfg.Class)
}

func (Provider) ServerTypeForClass(class string) string {
	cfg := core.Config{Provider: providerName, TargetOS: core.TargetLinux, Architecture: core.ArchitectureAMD64, Class: class}
	if candidates, matched := core.ProviderClassCandidatesForProfiles(classProfiles, cfg); matched {
		return candidates[0]
	}
	return cloudflareTypeForClass(class)
}

func cloudflareTypeForClass(class string) string {
	normalizedClass := strings.ToLower(strings.TrimSpace(class))
	if normalizedClass != class {
		cfg := core.Config{Provider: providerName, TargetOS: core.TargetLinux, Architecture: core.ArchitectureAMD64, Class: normalizedClass}
		if candidates, matched := core.ProviderClassCandidatesForProfiles(classProfiles, cfg); matched {
			return candidates[0]
		}
	}
	if class == "" {
		return "standard-4"
	}
	if instanceType, ok := core.NormalizeCloudflareContainerInstanceType(class); ok {
		return instanceType
	}
	return strings.TrimSpace(class)
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterCloudflareProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyCloudflareProviderFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.ServerType == "" {
		cfg.ServerType = (Provider{}).ServerTypeForConfig(cfg)
	}
	if cfg.ServerType == "" && core.IsCanonicalProviderClass(cfg.Class) {
		return nil, core.Exit(2, "provider=%s has no class profile for class=%s target=%s architecture=%s", providerName, cfg.Class, cfg.TargetOS, cfg.Architecture)
	}
	if normalized, ok := core.NormalizeCloudflareContainerInstanceType(cfg.ServerType); ok {
		cfg.ServerType = normalized
	} else if !cfg.ServerTypeExplicit {
		cfg.ServerType = (Provider{}).ServerTypeForConfig(cfg)
	} else {
		return nil, core.Exit(2, "cloudflare --type must be one of %s", strings.Join(core.CloudflareContainerInstanceTypes(), ", "))
	}
	return NewCloudflareBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return shared.ConfigureDoctor("cloudflare", func() (core.Backend, error) { return p.Configure(cfg, rt) })
}
