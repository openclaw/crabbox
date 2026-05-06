// Package semaphore implements a Crabbox provider that creates Semaphore CI
// jobs as warm testbox environments. Pure REST API — no sem-agent binary needed.
//
// Install: copy to internal/providers/semaphore/ and add to all/all.go
package semaphore

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return "semaphore" }
func (Provider) Aliases() []string { return []string{"sem"} }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        "semaphore",
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return registerFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if v, ok := values.(flagValues); ok {
		applyFlagOverrides(cfg, fs, v)
	}
	return nil
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return newBackend(p.Spec(), cfg, rt)
}
