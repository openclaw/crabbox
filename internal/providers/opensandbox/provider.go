package opensandbox

import (
	"flag"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }

func (Provider) ServerTypeForConfig(core.Config) string { return "" }
func (Provider) ServerTypeForClass(string) string       { return "" }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      "opensandbox",
		Kind:        core.ProviderKindDelegatedRun,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureArchiveSync, core.FeatureCleanup, core.FeatureRunSession},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterOpenSandboxProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyOpenSandboxProviderFlags(cfg, fs, values)
}

func (Provider) ValidateConfig(cfg core.Config) error {
	return validateOpenSandboxConfig(cfg)
}

func validateOpenSandboxConfig(cfg Config) error {
	if cfg.OpenSandbox.TimeoutSecs < 0 {
		return exit(2, "opensandbox timeoutSecs must be non-negative")
	}
	if cfg.OpenSandbox.ExecTimeoutSecs < 0 {
		return exit(2, "opensandbox execTimeoutSecs must be non-negative")
	}
	return nil
}

func validateOpenSandboxRunConfig(cfg Config) error {
	return validateOpenSandboxRequestConfig(cfg, RunRequest{})
}

func validateOpenSandboxRequestConfig(cfg Config, req RunRequest) error {
	required := openSandboxRunBudgetForConfig(cfg, req.NoSync, req.SyncOnly)
	if lifetime := openSandboxLifetimeForConfig(cfg); lifetime < required {
		return exit(2, "opensandbox effective lifetime %s must cover sync/command budget %s", lifetime, required)
	}
	return nil
}

func openSandboxCommandBudgetForConfig(cfg Config) time.Duration {
	execTimeout := cfg.OpenSandbox.ExecTimeoutSecs
	if execTimeout == 0 {
		execTimeout = openSandboxExecTimeoutSecs
	}
	return time.Duration(execTimeout)*time.Second + openSandboxExecGrace
}

func openSandboxRunBudgetForConfig(cfg Config, noSync, syncOnly bool) time.Duration {
	commandBudget := openSandboxCommandBudgetForConfig(cfg)
	// Even --no-sync runs one remote command to create the configured workdir.
	syncBudget := commandBudget
	if !noSync && cfg.Sync.Timeout > 0 {
		syncBudget = cfg.Sync.Timeout
	}
	if syncOnly {
		return syncBudget
	}
	return syncBudget + commandBudget
}

func openSandboxLifetimeForConfig(cfg Config) time.Duration {
	lifetime := time.Duration(0)
	for _, candidate := range []time.Duration{
		time.Duration(cfg.OpenSandbox.TimeoutSecs) * time.Second,
		cfg.TTL,
	} {
		if candidate > 0 && (lifetime == 0 || candidate < lifetime) {
			lifetime = candidate
		}
	}
	if lifetime == 0 {
		return openSandboxMinimumTTL
	}
	return lifetime
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = providerName
	return &openSandboxBackend{spec: p.Spec(), cfg: cfg, rt: rt}, nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "opensandbox doctor backend unavailable")
	}
	return doctor, nil
}
