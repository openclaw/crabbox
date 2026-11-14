package azuresandbox

import (
	"flag"
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "azure-sandbox"

func init() { core.RegisterProvider(Provider{}) }

type Provider struct{}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name: providerName, Family: "azure", Kind: core.ProviderKindDelegatedRun,
		Features: core.FeatureSet{core.FeatureShellScriptRun, core.FeatureRunSession, core.FeaturePauseResume, core.FeatureSessionDiskRetentionV1},
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
	if _, err := machineForConfig(cfg); err != nil {
		return nil, err
	}
	return &backend{cfg: cfg, rt: rt}, nil
}

func (Provider) BackendCapabilities() core.Backend { return &backend{} }

func (Provider) ClaimScope(cfg core.Config) string {
	c := cfg.AzureSandbox
	return "azure-sandbox:" + c.Region + ":" + c.Subscription + ":" + c.ResourceGroup + ":" + c.Group
}

// Generic classes retain their original shape, including the default beast.
// Larger disk-based allocations use the native --type selector.
func (Provider) ClassProfiles() []core.ProviderClassProfile {
	cpu := 2
	return core.UniformLinuxAMD64ClassProfiles(core.ProviderClassMachine{Type: "2cpu-4gib", Architecture: core.ProviderClassArchitectureAMD64, VCPU: &cpu, Memory: &core.ProviderMemory{Value: 4, Unit: core.ProviderMemoryUnitGiB}})
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
		return cfg.ServerType
	}
	return "2cpu-4gib"
}

func machineForConfig(cfg core.Config) (map[string]string, error) {
	typ := (Provider{}).ServerTypeForConfig(cfg)
	switch typ {
	case "2cpu-4gib":
		return map[string]string{"cpu": "2000m", "memory": "4096Mi"}, nil
	case "4cpu-8gib":
		if cfg.AzureSandbox.SnapshotID != "" {
			return nil, fmt.Errorf("ACA Sandbox snapshot restore inherits captured compute size; --type 4cpu-8gib requires a disk image")
		}
		return map[string]string{"cpu": "4000m", "memory": "8192Mi"}, nil
	default:
		return nil, fmt.Errorf("ACA Sandbox unsupported type %q; choose 2cpu-4gib or 4cpu-8gib", typ)
	}
}
