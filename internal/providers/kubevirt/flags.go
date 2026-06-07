package kubevirt

import (
	"flag"
	"path"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	Kubectl         *string
	Virtctl         *string
	Kubeconfig      *string
	Context         *string
	Namespace       *string
	Template        *string
	SSHUser         *string
	SSHKey          *string
	SSHPublicKey    *string
	SSHPort         *string
	WorkRoot        *string
	DeleteOnRelease *bool
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		Kubectl:         fs.String("kubevirt-kubectl", defaults.KubeVirt.Kubectl, "kubectl executable"),
		Virtctl:         fs.String("kubevirt-virtctl", defaults.KubeVirt.Virtctl, "virtctl executable"),
		Kubeconfig:      fs.String("kubevirt-kubeconfig", defaults.KubeVirt.Kubeconfig, "Kubernetes kubeconfig path"),
		Context:         fs.String("kubevirt-context", defaults.KubeVirt.Context, "Kubernetes context"),
		Namespace:       fs.String("kubevirt-namespace", defaults.KubeVirt.Namespace, "Kubernetes namespace"),
		Template:        fs.String("kubevirt-template", defaults.KubeVirt.Template, "KubeVirt VirtualMachine manifest template"),
		SSHUser:         fs.String("kubevirt-ssh-user", defaults.KubeVirt.SSHUser, "guest SSH user"),
		SSHKey:          fs.String("kubevirt-ssh-key", defaults.KubeVirt.SSHKey, "guest SSH private key"),
		SSHPublicKey:    fs.String("kubevirt-ssh-public-key", defaults.KubeVirt.SSHPublicKey, "guest SSH public key inserted into the template"),
		SSHPort:         fs.String("kubevirt-ssh-port", defaults.KubeVirt.SSHPort, "guest SSH port"),
		WorkRoot:        fs.String("kubevirt-work-root", defaults.KubeVirt.WorkRoot, "guest Crabbox work root"),
		DeleteOnRelease: fs.Bool("kubevirt-delete-on-release", defaults.KubeVirt.DeleteOnRelease, "delete the VM on release instead of stopping it"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "kubevirt-kubectl") {
		cfg.KubeVirt.Kubectl = core.ExpandUserPath(*v.Kubectl)
	}
	if core.FlagWasSet(fs, "kubevirt-virtctl") {
		cfg.KubeVirt.Virtctl = core.ExpandUserPath(*v.Virtctl)
	}
	if core.FlagWasSet(fs, "kubevirt-kubeconfig") {
		cfg.KubeVirt.Kubeconfig = core.ExpandUserPath(*v.Kubeconfig)
	}
	if core.FlagWasSet(fs, "kubevirt-context") {
		cfg.KubeVirt.Context = *v.Context
	}
	if core.FlagWasSet(fs, "kubevirt-namespace") {
		cfg.KubeVirt.Namespace = *v.Namespace
	}
	if core.FlagWasSet(fs, "kubevirt-template") {
		cfg.KubeVirt.Template = core.ExpandUserPath(*v.Template)
	}
	if core.FlagWasSet(fs, "kubevirt-ssh-user") {
		cfg.KubeVirt.SSHUser = *v.SSHUser
	}
	if core.FlagWasSet(fs, "kubevirt-ssh-key") {
		cfg.KubeVirt.SSHKey = core.ExpandUserPath(*v.SSHKey)
	}
	if core.FlagWasSet(fs, "kubevirt-ssh-public-key") {
		cfg.KubeVirt.SSHPublicKey = core.ExpandUserPath(*v.SSHPublicKey)
	}
	if core.FlagWasSet(fs, "kubevirt-ssh-port") {
		cfg.KubeVirt.SSHPort = *v.SSHPort
	}
	if core.FlagWasSet(fs, "kubevirt-work-root") {
		cfg.KubeVirt.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if core.FlagWasSet(fs, "kubevirt-delete-on-release") {
		cfg.KubeVirt.DeleteOnRelease = *v.DeleteOnRelease
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
