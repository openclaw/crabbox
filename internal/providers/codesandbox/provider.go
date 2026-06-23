package codesandbox

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return []string{"csb", "code-sandbox"} }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      providerFamily,
		Kind:        core.ProviderKindDelegatedRun,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureArchiveSync, core.FeatureCleanup, core.FeaturePauseResume, core.FeatureRunSession},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterCodeSandboxProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyCodeSandboxProviderFlags(cfg, fs, values)
}

func (Provider) ValidateConfig(cfg core.Config) error {
	return validateCodeSandboxConfig(cfg)
}

func validateCodeSandboxConfig(cfg Config) error {
	csb := cfg.CodeSandbox
	if strings.TrimSpace(csb.Workdir) == "" {
		return exit(2, "codesandbox workdir must not be empty")
	}
	cfg.Provider = providerName
	if _, err := codeSandboxWorkdir(cfg); err != nil {
		return err
	}
	if strings.TrimSpace(csb.BridgeCommand) == "" {
		return exit(2, "codesandbox bridgeCommand must not be empty")
	}
	if strings.TrimSpace(csb.SDKPackage) == "" {
		return exit(2, "codesandbox sdkPackage must not be empty")
	}
	if csb.HibernationTimeoutSecs < 0 {
		return exit(2, "codesandbox hibernationTimeoutSecs must be non-negative")
	}
	if csb.DoctorListLimit < 0 {
		return exit(2, "codesandbox doctorListLimit must be non-negative")
	}
	if csb.OperationTimeoutSecs < 0 {
		return exit(2, "codesandbox operationTimeoutSecs must be non-negative")
	}
	if privacy := strings.ToLower(strings.TrimSpace(csb.Privacy)); privacy != "" {
		switch privacy {
		case "public", "unlisted", "private", "public-hosts":
		default:
			return exit(2, "codesandbox privacy must be public, unlisted, private, or public-hosts")
		}
	}
	if tier := strings.ToLower(strings.TrimSpace(csb.VMTier)); tier != "" {
		switch tier {
		case "pico", "nano", "micro", "small", "medium", "large", "xlarge":
		default:
			return exit(2, "codesandbox vmTier must be pico, nano, micro, small, medium, large, or xlarge")
		}
	}
	return nil
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = providerName
	return &codeSandboxBackend{spec: p.Spec(), cfg: cfg, rt: rt}, nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, exit(2, "codesandbox doctor backend unavailable")
	}
	return doctor, nil
}
