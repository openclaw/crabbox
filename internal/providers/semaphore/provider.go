// Package semaphore implements a Crabbox provider that creates Semaphore CI
// jobs as warm testbox environments. Pure REST API; no sem-agent binary needed.
package semaphore

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) ClaimScope(cfg core.Config) string {
	host, err := normalizeSemaphoreHost(cfg.Semaphore.Host)
	project := strings.TrimSpace(cfg.Semaphore.Project)
	if err != nil || host == "" || project == "" {
		return ""
	}
	return "host:" + strings.ToLower(host) + "|project:" + project
}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Aliases:          []string{"sem"},
		Authentication:   core.DirectProviderAuthentication(core.ProviderAuthenticationAPIToken),
		Name:             "semaphore",
		Family:           "semaphore",
		Kind:             core.ProviderKindSSHLease,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterSemaphoreConfigFlags(fs, defaults.Semaphore)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	_, err := core.ApplyProviderConfigFlags[core.SemaphoreConfigFlagValues](cfg, fs, values, &cfg.Semaphore, "semaphore")
	return err
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return newBackend(p.Spec(), cfg, rt)
}
