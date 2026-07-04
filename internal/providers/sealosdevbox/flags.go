package sealosdevbox

import (
	"flag"
	"path"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	networkSSHGate  = "SSHGate"
	networkNodePort = "NodePort"
)

type flagValues struct {
	Kubectl         *string
	Kubeconfig      *string
	Context         *string
	Namespace       *string
	Image           *string
	TemplateID      *string
	CPU             *string
	Memory          *string
	StorageLimit    *string
	Network         *string
	SSHGatewayHost  *string
	SSHGatewayPort  *string
	SSHUser         *string
	WorkRoot        *string
	NodeHost        *string
	DeleteOnRelease *bool
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		Kubectl:         fs.String("sealos-devbox-kubectl", defaults.SealosDevbox.Kubectl, "kubectl executable"),
		Kubeconfig:      fs.String("sealos-devbox-kubeconfig", defaults.SealosDevbox.Kubeconfig, "Kubernetes kubeconfig path"),
		Context:         fs.String("sealos-devbox-context", defaults.SealosDevbox.Context, "Kubernetes context"),
		Namespace:       fs.String("sealos-devbox-namespace", defaults.SealosDevbox.Namespace, "Kubernetes namespace"),
		Image:           fs.String("sealos-devbox-image", defaults.SealosDevbox.Image, "Sealos DevBox image"),
		TemplateID:      fs.String("sealos-devbox-template-id", defaults.SealosDevbox.TemplateID, "Sealos DevBox template ID"),
		CPU:             fs.String("sealos-devbox-cpu", defaults.SealosDevbox.CPU, "Sealos DevBox CPU request"),
		Memory:          fs.String("sealos-devbox-memory", defaults.SealosDevbox.Memory, "Sealos DevBox memory request"),
		StorageLimit:    fs.String("sealos-devbox-storage-limit", defaults.SealosDevbox.StorageLimit, "Sealos DevBox storage limit"),
		Network:         fs.String("sealos-devbox-network", defaults.SealosDevbox.Network, "Sealos DevBox network mode: SSHGate or NodePort"),
		SSHGatewayHost:  fs.String("sealos-devbox-ssh-gateway-host", defaults.SealosDevbox.SSHGatewayHost, "Sealos SSHGate host"),
		SSHGatewayPort:  fs.String("sealos-devbox-ssh-gateway-port", defaults.SealosDevbox.SSHGatewayPort, "Sealos SSHGate port"),
		SSHUser:         fs.String("sealos-devbox-ssh-user", defaults.SealosDevbox.SSHUser, "DevBox SSH user"),
		WorkRoot:        fs.String("sealos-devbox-work-root", defaults.SealosDevbox.WorkRoot, "DevBox Crabbox work root"),
		NodeHost:        fs.String("sealos-devbox-node-host", defaults.SealosDevbox.NodeHost, "Node host for NodePort mode"),
		DeleteOnRelease: fs.Bool("sealos-devbox-delete-on-release", defaults.SealosDevbox.DeleteOnRelease, "delete the DevBox on release instead of retaining it"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "sealos-devbox-kubectl") {
		cfg.SealosDevbox.Kubectl = core.ExpandUserPath(*v.Kubectl)
	}
	if core.FlagWasSet(fs, "sealos-devbox-kubeconfig") {
		cfg.SealosDevbox.Kubeconfig = core.ExpandUserPath(*v.Kubeconfig)
	}
	if core.FlagWasSet(fs, "sealos-devbox-context") {
		cfg.SealosDevbox.Context = *v.Context
	}
	if core.FlagWasSet(fs, "sealos-devbox-namespace") {
		cfg.SealosDevbox.Namespace = *v.Namespace
	}
	if core.FlagWasSet(fs, "sealos-devbox-image") {
		cfg.SealosDevbox.Image = *v.Image
	}
	if core.FlagWasSet(fs, "sealos-devbox-template-id") {
		cfg.SealosDevbox.TemplateID = *v.TemplateID
	}
	if core.FlagWasSet(fs, "sealos-devbox-cpu") {
		cfg.SealosDevbox.CPU = *v.CPU
	}
	if core.FlagWasSet(fs, "sealos-devbox-memory") {
		cfg.SealosDevbox.Memory = *v.Memory
	}
	if core.FlagWasSet(fs, "sealos-devbox-storage-limit") {
		cfg.SealosDevbox.StorageLimit = *v.StorageLimit
	}
	if core.FlagWasSet(fs, "sealos-devbox-network") {
		cfg.SealosDevbox.Network = *v.Network
	}
	if core.FlagWasSet(fs, "sealos-devbox-ssh-gateway-host") {
		cfg.SealosDevbox.SSHGatewayHost = *v.SSHGatewayHost
	}
	if core.FlagWasSet(fs, "sealos-devbox-ssh-gateway-port") {
		cfg.SealosDevbox.SSHGatewayPort = *v.SSHGatewayPort
	}
	if core.FlagWasSet(fs, "sealos-devbox-ssh-user") {
		cfg.SealosDevbox.SSHUser = *v.SSHUser
	}
	if core.FlagWasSet(fs, "sealos-devbox-work-root") {
		cfg.SealosDevbox.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
		core.MarkSealosDevboxWorkRootExplicit(cfg)
	}
	if core.FlagWasSet(fs, "sealos-devbox-node-host") {
		cfg.SealosDevbox.NodeHost = *v.NodeHost
	}
	if core.FlagWasSet(fs, "sealos-devbox-delete-on-release") {
		cfg.SealosDevbox.DeleteOnRelease = *v.DeleteOnRelease
		core.MarkDeleteOnReleaseExplicit(cfg, providerName)
	}
	return validateBaseConfig(*cfg)
}

func validateBaseConfig(cfg core.Config) error {
	values := cfg.SealosDevbox
	for label, value := range map[string]string{
		"kubectl":   values.Kubectl,
		"context":   values.Context,
		"namespace": values.Namespace,
		"ssh user":  values.SSHUser,
	} {
		if strings.TrimSpace(value) == "" {
			return core.Exit(2, "sealos-devbox %s is required", label)
		}
	}
	if strings.ContainsAny(values.Namespace, " \t\r\n/") {
		return core.Exit(2, "sealos-devbox namespace %q is invalid", values.Namespace)
	}
	if strings.ContainsAny(values.SSHUser, " \t\r\n") {
		return core.Exit(2, "sealos-devbox SSH user %q is invalid", values.SSHUser)
	}
	if port := strings.TrimSpace(values.SSHGatewayPort); port != "" {
		parsed, err := strconv.Atoi(port)
		if err != nil || parsed < 1 || parsed > 65535 {
			return core.Exit(2, "sealos-devbox SSH gateway port must be between 1 and 65535")
		}
	}
	clean := path.Clean(sealosWorkRoot(cfg))
	if !strings.HasPrefix(clean, "/") {
		return core.Exit(2, "sealosDevbox.workRoot %q must resolve to an absolute path", values.WorkRoot)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var":
		return core.Exit(2, "sealosDevbox.workRoot %q is too broad; choose a dedicated subdirectory", clean)
	}
	return nil
}

func validateConfig(cfg core.Config) error {
	if err := validateBaseConfig(cfg); err != nil {
		return err
	}
	values := cfg.SealosDevbox
	switch normalizeNetwork(values.Network) {
	case networkSSHGate:
		if strings.TrimSpace(values.SSHGatewayHost) == "" {
			return core.Exit(2, "sealos-devbox.sshGatewayHost is required when network=SSHGate")
		}
		if strings.TrimSpace(values.SSHGatewayPort) == "" {
			return core.Exit(2, "sealos-devbox.sshGatewayPort is required when network=SSHGate")
		}
	case networkNodePort:
		if strings.TrimSpace(values.NodeHost) == "" {
			return core.Exit(2, "sealos-devbox.nodeHost is required when network=NodePort until live discovery is implemented")
		}
	default:
		return core.Exit(2, "sealos-devbox network must be SSHGate or NodePort")
	}
	return nil
}

func normalizeNetwork(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "sshgate", "ssh-gate", "ssh_gate":
		return networkSSHGate
	case "nodeport", "node-port", "node_port":
		return networkNodePort
	default:
		return strings.TrimSpace(value)
	}
}

func sealosWorkRoot(cfg core.Config) string {
	return core.EffectiveSealosDevboxWorkRoot(cfg)
}
