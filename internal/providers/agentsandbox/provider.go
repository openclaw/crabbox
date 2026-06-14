package agentsandbox

import (
	"flag"
	"path"
	"path/filepath"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      "agent-sandbox",
		Kind:        core.ProviderKindDelegatedRun,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureArchiveSync, core.FeatureCleanup},
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

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, core.Exit(2, "provider=%s supports target=linux only", providerName)
	}
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	return &backend{
		spec:      p.Spec(),
		cfg:       cfg,
		rt:        rt,
		newClient: newKubernetesClient,
	}, nil
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

func validateConfig(cfg core.Config) error {
	values := cfg.AgentSandbox
	for label, value := range map[string]string{
		"kubectl":   values.Kubectl,
		"context":   values.Context,
		"namespace": values.Namespace,
		"warmPool":  values.WarmPool,
		"workdir":   values.Workdir,
	} {
		if strings.TrimSpace(value) == "" {
			return core.Exit(2, "agent-sandbox %s is required", label)
		}
	}
	kubectl := strings.TrimSpace(values.Kubectl)
	if !filepath.IsAbs(kubectl) && (kubectl == "." || kubectl == ".." || strings.ContainsAny(kubectl, `/\`)) {
		return core.Exit(2, "agent-sandbox kubectl %q must be a bare executable name or absolute path", values.Kubectl)
	}
	if err := validateKubeconfigInputs(values.Kubeconfig); err != nil {
		return err
	}
	if strings.ContainsAny(values.Namespace, " \t\r\n/") {
		return core.Exit(2, "agent-sandbox namespace %q is invalid", values.Namespace)
	}
	if strings.ContainsAny(values.WarmPool, " \t\r\n/") {
		return core.Exit(2, "agent-sandbox warmPool %q is invalid", values.WarmPool)
	}
	if strings.ContainsAny(values.Container, " \t\r\n/") {
		return core.Exit(2, "agent-sandbox container %q is invalid", values.Container)
	}
	cleanWorkdir := path.Clean(values.Workdir)
	if !strings.HasPrefix(cleanWorkdir, "/") {
		return core.Exit(2, "agent-sandbox workdir %q must be absolute", values.Workdir)
	}
	switch cleanWorkdir {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var":
		return core.Exit(2, "agent-sandbox workdir %q is too broad; choose a dedicated subdirectory", cleanWorkdir)
	}
	if values.SandboxReadyTimeout < 0 {
		return core.Exit(2, "agent-sandbox sandboxReadyTimeout must be non-negative")
	}
	if values.PodReadyTimeout < 0 {
		return core.Exit(2, "agent-sandbox podReadyTimeout must be non-negative")
	}
	if values.ExecTimeoutSecs < 0 {
		return core.Exit(2, "agent-sandbox execTimeoutSecs must be non-negative")
	}
	return nil
}
