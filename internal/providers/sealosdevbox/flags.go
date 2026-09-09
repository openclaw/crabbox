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

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.SealosDevboxConfigFlagValues)
	if !ok {
		return nil
	}
	applied := v.Apply(&cfg.SealosDevbox, fs)
	if applied.Kubectl {
		cfg.SealosDevbox.Kubectl = core.ExpandUserPath(cfg.SealosDevbox.Kubectl)
	}
	if applied.Kubeconfig {
		cfg.SealosDevbox.Kubeconfig = core.ExpandUserPath(cfg.SealosDevbox.Kubeconfig)
	}
	if applied.WorkRoot {
		cfg.WorkRoot = cfg.SealosDevbox.WorkRoot
		core.MarkSealosDevboxWorkRootExplicit(cfg)
	}
	if applied.DeleteOnRelease {
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
