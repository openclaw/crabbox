package cloudflare

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return providerName }

func (Provider) Aliases() []string {
	return []string{providerAlias}
}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      "cloudflare",
		Kind:        core.ProviderKindDelegatedRun,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureArchiveSync, core.FeatureCleanup},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterCloudflareProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyCloudflareProviderFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.ServerType == "" {
		cfg.ServerType = core.CloudflareContainerInstanceTypeForClass(cfg.Class)
	}
	if normalized, ok := core.NormalizeCloudflareContainerInstanceType(cfg.ServerType); ok {
		cfg.ServerType = normalized
	} else if !cfg.ServerTypeExplicit {
		cfg.ServerType = core.CloudflareContainerInstanceTypeForClass(cfg.Class)
	} else {
		return nil, core.Exit(2, "cloudflare --type must be one of %s", strings.Join(core.CloudflareContainerInstanceTypes(), ", "))
	}
	return NewCloudflareBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "cloudflare doctor backend unavailable")
	}
	return doctor, nil
}
