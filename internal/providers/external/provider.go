package external

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "external"

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return []string{"exec-provider"} }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      "external",
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return registerFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return applyFlags(cfg, fs, values)
}

func (Provider) ValidateConfig(cfg core.Config) error {
	return validateConfig(cfg)
}

func (Provider) RouteConfig(cfg *core.Config, _ *flag.FlagSet, _ any) error {
	if cfg.WorkRoot == core.BaseConfig().WorkRoot && cfg.External.WorkRoot != "" {
		cfg.WorkRoot = cfg.External.WorkRoot
	}
	return nil
}

func (Provider) CommandRoutingArgs(cfg core.Config, leaseID string) []string {
	path := strings.TrimSpace(cfg.External.RoutingFile)
	if path == "" {
		var err error
		path, err = core.ExternalRoutingPath(leaseID)
		if err != nil {
			return nil
		}
	}
	return []string{"--external-routing-file", path}
}

func (Provider) ControllerProviderScope(cfg core.Config) (string, error) {
	return externalControllerScope(cfg)
}

func (Provider) SupportsControllerFixedLeaseID(cfg core.Config) bool {
	if !cfg.External.Capabilities.IdempotentLeaseID {
		return false
	}
	if !lifecycleConfigured(cfg.External) {
		return true
	}
	return lifecycleControllerIdentityAttestationConfigured(cfg.External)
}

func lifecycleControllerIdentityAttestationConfigured(cfg core.ExternalConfig) bool {
	// Fixed-ID provisioning is safe only when both sides of the lifecycle
	// return complete command-observed provider identity. Default connection
	// expansion is useful for interactive use, but is not release attestation.
	return lifecycleConfigured(cfg) &&
		cfg.Lifecycle.Acquire.Output == lifecycleOutputJSONLease &&
		lifecycleOperationConfigured(cfg.Lifecycle.Resolve) &&
		cfg.Lifecycle.Resolve.Output == lifecycleOutputJSONLease &&
		lifecycleOperationConfigured(cfg.Lifecycle.List) &&
		cfg.Lifecycle.List.Output == lifecycleOutputJSONLeaseArray &&
		lifecycleOperationConsumesRawCloudID(cfg.Lifecycle.Release)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, core.Exit(2, "provider=%s supports target=linux only", providerName)
	}
	cfg.Provider = providerName
	loadedRouting := core.ExternalRoutingLoaded(cfg.External)
	if path := strings.TrimSpace(cfg.External.RoutingFile); path != "" && !loadedRouting {
		routing, err := core.LoadExternalRouting(path)
		if err != nil {
			return nil, core.Exit(2, "%v", err)
		}
		cfg.External = routing
		core.MarkExternalRoutingCredentialSources(&cfg)
		loadedRouting = true
	}
	if err := core.ValidateProviderCredentialDestination(cfg); err != nil {
		return nil, err
	}
	base := core.BaseConfig()
	explicitTopLevelWorkRoot := !loadedRouting && strings.TrimSpace(cfg.WorkRoot) != "" && cfg.WorkRoot != base.WorkRoot
	providerWorkRootDefault := strings.TrimSpace(cfg.External.WorkRoot) == "" || cfg.External.WorkRoot == base.External.WorkRoot
	if explicitTopLevelWorkRoot && providerWorkRootDefault {
		cfg.External.WorkRoot = cfg.WorkRoot
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	cfg.TargetOS = core.TargetLinux
	cfg.SSHFallbackPorts = nil
	cfg.WorkRoot = externalWorkRoot(cfg)
	return &leaseBackend{spec: p.Spec(), cfg: cfg, rt: rt}, nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	return backend.(core.DoctorBackend), nil
}
