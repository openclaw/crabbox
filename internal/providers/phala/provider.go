package phala

import (
	"flag"
	"path"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return []string{"phala-cloud", "dstack"} }

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
	instanceType := strings.TrimSpace((Provider{}).ServerTypeForConfig(cfg))
	if prefix, _, ok := strings.Cut(instanceType, ":"); ok && !strings.HasPrefix(strings.ToLower(prefix), "linux/") {
		return core.Exit(2, "provider=%s supports Linux instance types only, got %q", providerName, instanceType)
	}
	workRoot := strings.TrimSpace(cfg.Phala.WorkRoot)
	cleanWorkRoot := path.Clean(workRoot)
	if workRoot == "" || !strings.HasPrefix(cleanWorkRoot, "/") || cleanWorkRoot != workRoot {
		return core.Exit(2, "phala.workRoot must be a canonical absolute Linux path")
	}
	switch cleanWorkRoot {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/work":
		return core.Exit(2, "phala.workRoot %q is too broad; choose a dedicated subdirectory", cleanWorkRoot)
	}
	if compose := strings.TrimSpace(cfg.Phala.Compose); compose != "" {
		cleanCompose := path.Clean(compose)
		if strings.Contains(compose, "://") {
			return core.Exit(2, "phala.compose must be a local file path, not a URL")
		}
		if cleanCompose == "." || cleanCompose == ".." || strings.HasPrefix(cleanCompose, "../") {
			return core.Exit(2, "phala.compose %q must not escape the working directory", compose)
		}
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
	if cfg.Phala.InstanceType != "" {
		return cfg.Phala.InstanceType
	}
	return instanceTypeForClass(cfg.Class)
}

func (Provider) ServerTypeForClass(class string) string {
	return instanceTypeForClass(class)
}
