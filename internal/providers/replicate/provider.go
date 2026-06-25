package replicate

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
		Family:      "replicate",
		Kind:        core.ProviderKindDelegatedRun,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureArchiveSync, core.FeatureRunSession},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterReplicateProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyReplicateProviderFlags(cfg, fs, values)
}

func (Provider) ValidateConfig(cfg core.Config) error {
	return ValidateConfig(cfg)
}

func (p Provider) Configure(core.Config, core.Runtime) (core.Backend, error) {
	return replicateBackend{spec: p.Spec()}, nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "replicate doctor backend unavailable")
	}
	return doctor, nil
}

type replicateBackend struct {
	spec core.ProviderSpec
}

func (b replicateBackend) Spec() core.ProviderSpec { return b.spec }

func (b replicateBackend) Doctor(context.Context, core.DoctorRequest) (core.DoctorResult, error) {
	return core.DoctorResult{
		Provider: providerName,
		Message:  "auth=unchecked control_plane=unchecked inventory=unchecked api=not-implemented mutation=false leases=0 runtime=unchecked",
		Status:   "not-implemented",
	}, nil
}

func (b replicateBackend) Warmup(context.Context, core.WarmupRequest) error {
	return exit(2, "provider=replicate backend lifecycle is not implemented in this build")
}

func (b replicateBackend) Run(context.Context, core.RunRequest) (core.RunResult, error) {
	return core.RunResult{}, exit(2, "provider=replicate backend lifecycle is not implemented in this build")
}

func (b replicateBackend) List(context.Context, core.ListRequest) ([]core.LeaseView, error) {
	return nil, exit(2, "provider=replicate backend lifecycle is not implemented in this build")
}

func (b replicateBackend) Status(context.Context, core.StatusRequest) (core.StatusView, error) {
	return core.StatusView{}, exit(2, "provider=replicate backend lifecycle is not implemented in this build")
}

func (b replicateBackend) Stop(context.Context, core.StopRequest) error {
	return exit(2, "provider=replicate backend lifecycle is not implemented in this build")
}
