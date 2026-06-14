package lambda

import (
	"context"
	"flag"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
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

func (Provider) ValidateConfig(cfg core.Config) error {
	return validateConfig(cfg)
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	return typeForConfig(cfg)
}

func (Provider) ServerTypeForClass(class string) string {
	return serverTypeForClass(class)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return &backend{spec: p.Spec(), cfg: cfg, rt: rt, clientFactory: newLambdaAPIClient}, nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return &backend{spec: p.Spec(), cfg: cfg, rt: rt, clientFactory: newLambdaAPIClient}, nil
}

type backend struct {
	shared.DirectSSHBackend
	spec          core.ProviderSpec
	cfg           core.Config
	rt            core.Runtime
	clientFactory func(core.Runtime) (lambdaAPI, error)
	waitSSH       func(context.Context, *core.SSHTarget, string, time.Duration) error
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func newLambdaAPIClient(rt core.Runtime) (lambdaAPI, error) {
	return newClient(rt)
}
