package opensandbox

import (
	"flag"
	"math"
	"os"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) DiagnosticSecrets(core.Config) []string {
	return []string{
		os.Getenv("CRABBOX_OPENSANDBOX_API_KEY"),
		os.Getenv("OPEN_SANDBOX_API_KEY"),
	}
}

func (Provider) ServerTypeForConfig(core.Config) string { return "" }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Authentication:             core.DirectProviderAuthentication(core.ProviderAuthenticationAPIKey),
		SyncGuardrailFullCandidate: true,
		Name:                       providerName,
		Family:                     "opensandbox",
		Kind:                       core.ProviderKindDelegatedRun,
		Targets:                    []core.TargetSpec{{OS: core.TargetLinux}},
		Features:                   core.FeatureSet{core.FeatureArchiveSync, core.FeatureCleanup, core.FeatureRunSession},
		Coordinator:                core.CoordinatorNever,
		ClassDisposition:           core.ProviderClassDispositionUnmapped,
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

func validateOpenSandboxConfig(cfg core.Config) error {
	if cfg.OpenSandbox.TimeoutSecs < 0 {
		return core.Exit(2, "opensandbox timeoutSecs must be non-negative")
	}
	if cfg.OpenSandbox.ExecTimeoutSecs < 0 {
		return core.Exit(2, "opensandbox execTimeoutSecs must be non-negative")
	}
	return nil
}

func validateOpenSandboxRunConfig(cfg core.Config) error {
	return validateOpenSandboxRequestConfig(cfg, core.RunRequest{})
}

func validateOpenSandboxRequestConfig(cfg core.Config, req core.RunRequest) error {
	required, err := openSandboxRunBudgetForConfig(cfg, req.NoSync, req.SyncOnly)
	if err != nil {
		return err
	}
	lifetime, err := openSandboxLifetimeForConfig(cfg)
	if err != nil {
		return err
	}
	if _, err := durationSecondsCeil(lifetime); err != nil {
		return err
	}
	if lifetime < required {
		return core.Exit(2, "opensandbox effective lifetime %s must cover sync/command budget %s", lifetime, required)
	}
	return nil
}

func openSandboxExecutionBudget(seconds int) (time.Duration, error) {
	if seconds < 0 {
		return 0, core.Exit(2, "opensandbox execTimeoutSecs must be non-negative")
	}
	if seconds == 0 {
		return 0, nil
	}
	if budget, ok := shared.SecondsWithGrace(int64(seconds), openSandboxExecGrace); ok {
		return budget, nil
	}
	return 0, core.Exit(2, "opensandbox execution timeout exceeds the supported request budget")
}

func openSandboxCommandBudgetForConfig(cfg core.Config) (time.Duration, error) {
	seconds := cfg.OpenSandbox.ExecTimeoutSecs
	if seconds == 0 {
		seconds = openSandboxExecTimeoutSecs
	}
	return openSandboxExecutionBudget(seconds)
}

func openSandboxRunBudgetForConfig(cfg core.Config, noSync, syncOnly bool) (time.Duration, error) {
	commandBudget, err := openSandboxCommandBudgetForConfig(cfg)
	if err != nil {
		return 0, err
	}
	// Even --no-sync requires a remote command to create the workdir.
	syncBudget := commandBudget
	if !noSync && cfg.Sync.Timeout > 0 {
		syncBudget = cfg.Sync.Timeout
	}
	if syncOnly {
		return syncBudget, nil
	}
	if syncBudget > time.Duration(math.MaxInt64)-commandBudget {
		return 0, core.Exit(2, "opensandbox combined sync/command budget exceeds the supported duration")
	}
	return syncBudget + commandBudget, nil
}

func openSandboxLifetimeForConfig(cfg core.Config) (time.Duration, error) {
	providerLifetime, ok := shared.SecondsWithGrace(int64(cfg.OpenSandbox.TimeoutSecs), 0)
	if !ok {
		return 0, core.Exit(2, "opensandbox timeoutSecs exceeds the supported lifetime")
	}
	lifetime := time.Duration(0)
	for _, candidate := range []time.Duration{providerLifetime, cfg.TTL} {
		if candidate > 0 && (lifetime == 0 || candidate < lifetime) {
			lifetime = candidate
		}
	}
	if lifetime == 0 {
		return openSandboxMinimumTTL, nil
	}
	return lifetime, nil
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = providerName
	return &openSandboxBackend{spec: p.Spec(), cfg: cfg, rt: rt}, nil
}
