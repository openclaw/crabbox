package linode

import (
	"context"
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "linode"

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
	return linodeServerTypeForClass(cfg.Class)
}

func (Provider) ServerTypeForClass(class string) string {
	return linodeServerTypeForClass(class)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return linodeFoundationBackend{spec: p.Spec()}, nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return linodeFoundationBackend{spec: p.Spec()}, nil
}

type linodeFoundationBackend struct {
	spec core.ProviderSpec
}

func (b linodeFoundationBackend) Spec() core.ProviderSpec {
	return b.spec
}

func (b linodeFoundationBackend) Doctor(ctx context.Context, req core.DoctorRequest) (core.DoctorResult, error) {
	return core.DoctorResult{Provider: providerName, Status: "ok", Message: "provider foundation registered; lifecycle implemented in a later plan", Checks: []core.DoctorCheck{{
		Check:   "linode-provider-foundation",
		Status:  "ok",
		Message: "provider foundation registered; lifecycle implemented in a later plan",
	}}}, nil
}
