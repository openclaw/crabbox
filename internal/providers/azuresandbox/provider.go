package azuresandbox

import (
	"flag"
	"fmt"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "azure-sandbox"

func init() { core.RegisterProvider(Provider{}) }

type Provider struct{}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name: providerName, Family: "azure", Kind: core.ProviderKindDelegatedRun,
		Features: core.FeatureSet{core.FeatureShellScriptRun, core.FeatureRunSession},
		Targets:  []core.TargetSpec{{OS: core.TargetLinux}}, Coordinator: core.CoordinatorNever,
		Authentication:   core.DirectProviderAuthentication(core.ProviderAuthenticationCLI),
		ClassDisposition: core.ProviderClassDispositionMapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, cfg core.Config) any {
	return core.RegisterAzureSandboxConfigFlags(fs, cfg.AzureSandbox)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	_, err := core.ApplyProviderConfigFlags[core.AzureSandboxConfigFlagValues](cfg, fs, values, &cfg.AzureSandbox, providerName)
	return err
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, fmt.Errorf("ACA Sandbox supports Linux only")
	}
	return &backend{cfg: cfg, rt: rt}, nil
}

func (Provider) BackendCapabilities() core.Backend { return &backend{} }

func (Provider) ClaimScope(cfg core.Config) string {
	c := cfg.AzureSandbox
	return "azure-sandbox:" + c.Region + ":" + c.Subscription + ":" + c.ResourceGroup + ":" + c.Group
}

// All generic classes currently resolve to the one configured initial shape.
// This follows the common fixed-shape catalog contract; it claims no larger SKU.
func (Provider) ClassProfiles() []core.ProviderClassProfile {
	cpu := 2
	return core.UniformLinuxAMD64ClassProfiles(core.ProviderClassMachine{Type: "2cpu-4gib", Architecture: core.ProviderClassArchitectureAMD64, VCPU: &cpu, Memory: &core.ProviderMemory{Value: 4, Unit: core.ProviderMemoryUnitGiB}})
}

func (Provider) ServerTypeForConfig(core.Config) string { return "2cpu-4gib" }
