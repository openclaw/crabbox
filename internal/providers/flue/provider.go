package flue

import (
	"context"
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

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
		Kind:        core.ProviderKindDelegatedRun,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureArchiveSync},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterFlueProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyFlueProviderFlags(cfg, fs, values)
}

func (Provider) ValidateConfig(cfg core.Config) error {
	return ValidateFlueConfig(cfg)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	return &backend{spec: p.Spec(), cfg: cfg, rt: rt}, nil
}

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Warmup(context.Context, WarmupRequest) error {
	return unsupported()
}

func (b *backend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{}, unsupported()
}

func (b *backend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, unsupported()
}

func (b *backend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{}, unsupported()
}

func (b *backend) Stop(context.Context, StopRequest) error {
	return unsupported()
}

func unsupported() error {
	return exit(2, "provider=%s run bridge is not implemented yet; this build only exposes the provider contract", providerName)
}
