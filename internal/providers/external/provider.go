package external

import (
	"flag"
	"fmt"
	"os"
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
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}, {OS: core.TargetMacOS}, {OS: core.TargetWindows, WindowsMode: "normal"}, {OS: core.TargetWindows, WindowsMode: "wsl2"}},
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

func (Provider) ResolveDesktopCredentials(cfg core.Config, target core.SSHTarget) (core.DesktopCredentials, bool, error) {
	desktop := cfg.External.Connection.Desktop
	passwordEnv := strings.TrimSpace(desktop.PasswordEnv)
	if passwordEnv == "" {
		return core.DesktopCredentials{}, false, nil
	}
	password, ok := os.LookupEnv(passwordEnv)
	if !ok || strings.TrimSpace(password) == "" {
		return core.DesktopCredentials{}, false, fmt.Errorf("external desktop password environment variable %s is unset or empty", passwordEnv)
	}
	username := strings.TrimSpace(desktop.Username)
	if username == "" {
		username = strings.TrimSpace(target.User)
	}
	if username == "" {
		username = strings.TrimSpace(cfg.External.Connection.SSH.User)
	}
	return core.DesktopCredentials{Username: username, Password: password}, true, nil
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
	args := []string{"--external-routing-file", path}
	if username := strings.TrimSpace(cfg.External.Connection.Desktop.Username); username != "" {
		args = append(args, "--external-desktop-username", username)
	}
	if passwordEnv := strings.TrimSpace(cfg.External.Connection.Desktop.PasswordEnv); passwordEnv != "" {
		args = append(args, "--external-desktop-password-env", passwordEnv)
	}
	return args
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
	if cfg.TargetOS == "" {
		cfg.TargetOS = core.TargetLinux
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
		core.ApplyExternalDesktopEnvironmentOverrides(&cfg)
		loadedRouting = true
	}
	if loadedRouting {
		targetOS, windowsMode := core.ExternalRoutingTarget(cfg.External)
		if !core.IsTargetExplicit(&cfg) {
			cfg.TargetOS = targetOS
		}
		if !core.IsWindowsModeExplicit(&cfg) {
			cfg.WindowsMode = core.WindowsModeNormal
			if cfg.TargetOS == core.TargetWindows {
				cfg.WindowsMode = windowsMode
			}
		}
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
	cfg.Provider = providerName
	cfg.SSHFallbackPorts = nil
	cfg.WorkRoot = externalWorkRoot(cfg)
	core.SetExternalRoutingTarget(&cfg.External, cfg.TargetOS, cfg.WindowsMode)
	return &leaseBackend{spec: p.Spec(), cfg: cfg, rt: rt}, nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	return backend.(core.DoctorBackend), nil
}
