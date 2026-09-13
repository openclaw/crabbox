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

func (Provider) Name() string      { return "semaphore" }
func (Provider) Aliases() []string { return []string{"sem"} }

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
	if v, ok := values.(core.SemaphoreConfigFlagValues); ok {
		applied, err := v.Apply(&cfg.Semaphore, fs)
		core.RecordProviderFlagInputs(cfg, applied.InputAccepted, "semaphore")
		if err != nil {
			return err
		}
	}
	return nil
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return newBackend(p.Spec(), cfg, rt)
}
