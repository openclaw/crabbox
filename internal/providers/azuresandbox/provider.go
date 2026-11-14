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
// Other disk-based sizes use the native --type selector.
func (Provider) ClassProfiles() []core.ProviderClassProfile {
	cpu := 2
	return core.UniformLinuxAMD64ClassProfiles(core.ProviderClassMachine{Type: "2cpu-4gib", Architecture: core.ProviderClassArchitectureAMD64, VCPU: &cpu, Memory: &core.ProviderMemory{Value: 4, Unit: core.ProviderMemoryUnitGiB}})
}

// Azure resource tiers: https://sandboxes.azure.com/docs/sandboxes/sandbox/#resource-tiers
// Keep the original L shape as the Crabbox default, independently of Azure defaults.
var sandboxMachines = []struct {
	typ, tier, cpu, memory, disk string
}{
	{"0.25cpu-0.5gib", "XS", "250m", "512Mi", "5Gi"},
	{"0.5cpu-1gib", "S", "500m", "1024Mi", "10Gi"},
	{"1cpu-2gib", "M", "1000m", "2048Mi", "20Gi"},
	{"2cpu-4gib", "L", "2000m", "4096Mi", "40Gi"},
	{"4cpu-8gib", "XL", "4000m", "8192Mi", "80Gi"},
	{"8cpu-16gib", "2XL", "8000m", "16384Mi", "160Gi"},
	{"16cpu-32gib", "3XL", "16000m", "32768Mi", "320Gi"},
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
		typ := strings.TrimSpace(cfg.ServerType)
		for _, machine := range sandboxMachines {
			if strings.EqualFold(typ, machine.typ) || strings.EqualFold(typ, machine.tier) {
				return machine.typ
			}
		}
		return typ
	}
	return "2cpu-4gib"
}

func machineForConfig(cfg core.Config) (map[string]string, error) {
	typ := (Provider{}).ServerTypeForConfig(cfg)
	choices := make([]string, 0, len(sandboxMachines))
	for _, machine := range sandboxMachines {
		if typ == machine.typ {
			if typ != "2cpu-4gib" && cfg.AzureSandbox.SnapshotID != "" {
				return nil, fmt.Errorf("ACA Sandbox snapshot restore inherits captured compute size; --type %s requires a disk image", typ)
			}
			resources := map[string]string{"cpu": machine.cpu, "memory": machine.memory}
			// Explicit tiers include their documented disk capacity. Leave legacy
			// implicit defaults and snapshot restores byte-for-byte unchanged.
			if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" && cfg.AzureSandbox.SnapshotID == "" {
				resources["disk"] = machine.disk
			}
			return resources, nil
		}
		choices = append(choices, machine.typ+" ("+machine.tier+")")
	}
	return nil, fmt.Errorf("ACA Sandbox unsupported type %q; choose %s", typ, strings.Join(choices, ", "))
}
