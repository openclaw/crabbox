package kubevirt

import (
	"flag"
	"path"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.KubeVirtConfigFlagValues)
	if !ok {
		return nil
	}
	applied := v.Apply(&cfg.KubeVirt, fs)
	if applied.Kubectl {
		cfg.KubeVirt.Kubectl = core.ExpandUserPath(cfg.KubeVirt.Kubectl)
	}
	if applied.Virtctl {
		cfg.KubeVirt.Virtctl = core.ExpandUserPath(cfg.KubeVirt.Virtctl)
	}
	if applied.Kubeconfig {
		cfg.KubeVirt.Kubeconfig = core.ExpandUserPath(cfg.KubeVirt.Kubeconfig)
	}
	if applied.Template {
		cfg.KubeVirt.Template = core.ExpandUserPath(cfg.KubeVirt.Template)
	}
	if applied.SSHKey {
		cfg.KubeVirt.SSHKey = core.ExpandUserPath(cfg.KubeVirt.SSHKey)
	}
	if applied.SSHPublicKey {
		cfg.KubeVirt.SSHPublicKey = core.ExpandUserPath(cfg.KubeVirt.SSHPublicKey)
	}
	if applied.WorkRoot {
		cfg.WorkRoot = cfg.KubeVirt.WorkRoot
	}
	if applied.DeleteOnRelease {
		core.MarkDeleteOnReleaseExplicit(cfg, providerName)
	}
	return validateConfig(*cfg)
}

func validateConfig(cfg core.Config) error {
	for label, value := range map[string]string{
		"kubectl":   cfg.KubeVirt.Kubectl,
		"virtctl":   cfg.KubeVirt.Virtctl,
		"context":   cfg.KubeVirt.Context,
		"namespace": cfg.KubeVirt.Namespace,
		"ssh user":  cfg.KubeVirt.SSHUser,
	} {
		if strings.TrimSpace(value) == "" {
			return core.Exit(2, "kubevirt %s is required", label)
		}
	}
	if strings.ContainsAny(cfg.KubeVirt.Namespace, " \t\r\n/") {
		return core.Exit(2, "kubevirt namespace %q is invalid", cfg.KubeVirt.Namespace)
	}
	if strings.ContainsAny(cfg.KubeVirt.SSHUser, " \t\r\n") {
		return core.Exit(2, "kubevirt SSH user %q is invalid", cfg.KubeVirt.SSHUser)
	}
	port, err := strconv.Atoi(strings.TrimSpace(cfg.KubeVirt.SSHPort))
	if err != nil || port < 1 || port > 65535 {
		return core.Exit(2, "kubevirt SSH port must be between 1 and 65535")
	}
	clean := path.Clean(kubeVirtWorkRoot(cfg))
	if !strings.HasPrefix(clean, "/") {
		return core.Exit(2, "kubevirt.workRoot %q must resolve to an absolute path", cfg.KubeVirt.WorkRoot)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var":
		return core.Exit(2, "kubevirt.workRoot %q is too broad; choose a dedicated subdirectory", clean)
	}
	return nil
}

func validateAcquireConfig(cfg core.Config) error {
	if strings.TrimSpace(cfg.KubeVirt.Template) == "" {
		return core.Exit(2, "kubevirt template is required for acquisition")
	}
	return nil
}

func kubeVirtWorkRoot(cfg core.Config) string {
	return core.Blank(strings.TrimSpace(cfg.KubeVirt.WorkRoot), "/home/crabbox/crabbox")
}
