package applevm

import (
	"flag"
	"strings"

	"github.com/openclaw/crabbox/internal/applevmhelper"
	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "apple-vm"

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return providerName }

// The provider was named apple-vz before the vz library was replaced with
// the native VM daemon; the old names stay routable for existing configs.
func (Provider) Aliases() []string { return []string{"applevm", "apple-vz", "applevz"} }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      "local-vm",
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return registerFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return applyFlags(cfg, fs, values)
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	return applevmhelper.ImageIdentity(strings.TrimSpace(cfg.AppleVM.Image), cfg.AppleVM.ImageSHA256)
}

func (Provider) ServerTypeForClass(string) string { return "" }

func (Provider) ValidateConfig(cfg core.Config) error {
	if err := validateConfigBeforeDefaults(cfg); err != nil {
		return err
	}
	applyDefaults(&cfg)
	return validateConfig(cfg)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := validateConfigBeforeDefaults(cfg); err != nil {
		return nil, err
	}
	applyDefaults(&cfg)
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, core.Exit(2, "provider=%s supports target=linux only", providerName)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, core.Exit(2, "--tailscale is not supported for provider=%s; use a remote SSH provider when tailnet reachability is required", providerName)
	}
	return newBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "%s doctor backend unavailable", providerName)
	}
	return doctor, nil
}

func validateConfigBeforeDefaults(cfg core.Config) error {
	if cfg.AppleVM.CPUs < 0 || (cfg.AppleVM.CPUs == 0 && core.AppleVMCPUsExplicit(cfg)) {
		return core.Exit(2, "appleVM.cpus must be positive (got %d)", cfg.AppleVM.CPUs)
	}
	if cfg.AppleVM.MemoryMiB < 0 || (cfg.AppleVM.MemoryMiB == 0 && core.AppleVMMemoryExplicit(cfg)) {
		return core.Exit(2, "appleVM.memoryMiB must be at least 1024 MiB (got %d)", cfg.AppleVM.MemoryMiB)
	}
	if cfg.AppleVM.MemoryMiB > 0 && cfg.AppleVM.MemoryMiB < 1024 {
		return core.Exit(2, "appleVM.memoryMiB must be at least 1024 MiB (got %d)", cfg.AppleVM.MemoryMiB)
	}
	if cfg.AppleVM.DiskGiB < 0 || (cfg.AppleVM.DiskGiB == 0 && core.AppleVMDiskExplicit(cfg)) {
		return core.Exit(2, "appleVM.diskGiB must be positive (got %d)", cfg.AppleVM.DiskGiB)
	}
	return nil
}

func validateConfig(cfg core.Config) error {
	if err := applevmhelper.ValidatePOSIXAccountName(cfg.AppleVM.User); err != nil {
		return core.Exit(2, "appleVM.user %s", err)
	}
	if err := applevmhelper.ValidatePOSIXWorkRoot(cfg.AppleVM.WorkRoot); err != nil {
		return core.Exit(2, "appleVM.workRoot %s", err)
	}
	if cfg.AppleVM.MemoryMiB < 1024 {
		return core.Exit(2, "appleVM.memoryMiB must be at least 1024 MiB (got %d)", cfg.AppleVM.MemoryMiB)
	}
	if cfg.AppleVM.CPUs <= 0 {
		return core.Exit(2, "appleVM.cpus must be positive (got %d)", cfg.AppleVM.CPUs)
	}
	if cfg.AppleVM.DiskGiB <= 0 {
		return core.Exit(2, "appleVM.diskGiB must be positive (got %d)", cfg.AppleVM.DiskGiB)
	}
	return nil
}
