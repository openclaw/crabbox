package namespaceinstance

import (
	"flag"
	"net/url"
	"path"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return []string{"namespace-compute"} }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      providerName,
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

func (Provider) ValidateConfig(cfg core.Config) error {
	if endpoint := strings.TrimSpace(cfg.NamespaceInstance.Endpoint); endpoint != "" {
		parseValue := endpoint
		if !strings.Contains(parseValue, "://") {
			parseValue = "https://" + parseValue
		}
		parsed, err := url.Parse(parseValue)
		if err != nil || parsed.Host == "" {
			return core.Exit(2, "invalid Namespace endpoint")
		}
		if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return core.Exit(2, "Namespace endpoint must not contain credentials, query parameters, or fragments")
		}
	}
	machineType := strings.TrimSpace((Provider{}).ServerTypeForConfig(cfg))
	if prefix, _, ok := strings.Cut(machineType, ":"); ok && !strings.HasPrefix(strings.ToLower(prefix), "linux/") {
		return core.Exit(2, "provider=%s supports Linux machine types only, got %q", providerName, machineType)
	}
	workRoot := strings.TrimSpace(cfg.NamespaceInstance.WorkRoot)
	cleanWorkRoot := path.Clean(workRoot)
	if workRoot == "" || !strings.HasPrefix(cleanWorkRoot, "/") || cleanWorkRoot != workRoot {
		return core.Exit(2, "namespaceInstance.workRoot must be a canonical absolute Linux path")
	}
	switch cleanWorkRoot {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/work":
		return core.Exit(2, "namespaceInstance.workRoot %q is too broad; choose a dedicated subdirectory", cleanWorkRoot)
	}
	for _, volume := range cfg.NamespaceInstance.Volumes {
		if err := validateVolumeSpec(volume); err != nil {
			return err
		}
	}
	return nil
}

func validateVolumeSpec(spec string) error {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	parts := strings.Split(spec, ":")
	if len(parts) != 4 {
		return core.Exit(2, "Namespace volume %q must use kind:tag:mountpoint:size", spec)
	}
	switch parts[0] {
	case "cache", "persistent":
	default:
		return core.Exit(2, "Namespace volume %q kind must be cache or persistent", spec)
	}
	if strings.TrimSpace(parts[1]) == "" {
		return core.Exit(2, "Namespace volume %q tag is required", spec)
	}
	mountpoint := strings.TrimSpace(parts[2])
	if mountpoint == "" || !strings.HasPrefix(mountpoint, "/") || path.Clean(mountpoint) != mountpoint {
		return core.Exit(2, "Namespace volume %q mountpoint must be a canonical absolute Linux path", spec)
	}
	if strings.TrimSpace(parts[3]) == "" {
		return core.Exit(2, "Namespace volume %q size is required", spec)
	}
	return nil
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, core.Exit(2, "provider=%s supports target=linux only", providerName)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, core.Exit(2, "--tailscale is not supported for provider=%s", providerName)
	}
	applyDefaults(&cfg)
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	return &backend{spec: p.Spec(), cfg: cfg, rt: rt}, nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	return backend.(core.DoctorBackend), nil
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && cfg.ServerType != "" {
		return cfg.ServerType
	}
	if cfg.NamespaceInstance.MachineType != "" {
		return cfg.NamespaceInstance.MachineType
	}
	return machineTypeForClass(cfg.Class)
}

func (Provider) ServerTypeForClass(class string) string {
	return machineTypeForClass(class)
}
