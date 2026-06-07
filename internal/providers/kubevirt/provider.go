package kubevirt

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "kubevirt"

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return []string{"kubernetes-vm"} }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      "kubernetes",
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

func (Provider) RouteConfig(cfg *core.Config, _ *flag.FlagSet, _ any) error {
	if cfg.WorkRoot == core.BaseConfig().WorkRoot && cfg.KubeVirt.WorkRoot != "" {
		cfg.WorkRoot = cfg.KubeVirt.WorkRoot
	}
	return nil
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, core.Exit(2, "provider=%s supports target=linux only", providerName)
	}
	base := core.BaseConfig()
	explicitTopLevelWorkRoot := strings.TrimSpace(cfg.WorkRoot) != "" && cfg.WorkRoot != base.WorkRoot
	providerWorkRootDefault := strings.TrimSpace(cfg.KubeVirt.WorkRoot) == "" || cfg.KubeVirt.WorkRoot == base.KubeVirt.WorkRoot
	if explicitTopLevelWorkRoot && providerWorkRootDefault {
		cfg.KubeVirt.WorkRoot = cfg.WorkRoot
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.SSHUser = cfg.KubeVirt.SSHUser
	cfg.SSHPort = cfg.KubeVirt.SSHPort
	cfg.SSHKey = cfg.KubeVirt.SSHKey
	cfg.SSHFallbackPorts = nil
	cfg.WorkRoot = kubeVirtWorkRoot(cfg)
	return &leaseBackend{spec: p.Spec(), cfg: cfg, rt: rt}, nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	return backend.(core.DoctorBackend), nil
}
