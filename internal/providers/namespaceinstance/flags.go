package namespaceinstance

import (
	"flag"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	MachineType *string
	Duration    *string
	Ephemeral   *bool
	Region      *string
	Endpoint    *string
	Keychain    *string
	Volumes     *string
	WorkRoot    *string
}

func RegisterProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		MachineType: fs.String("namespace-instance-machine-type", defaults.NamespaceInstance.MachineType, "Namespace Compute machine type"),
		Duration:    fs.String("namespace-instance-duration", defaults.NamespaceInstance.Duration.String(), "Namespace Compute lease duration"),
		Ephemeral:   fs.Bool("namespace-instance-ephemeral", defaults.NamespaceInstance.Ephemeral, "create an ephemeral Namespace Compute instance"),
		Region:      fs.String("namespace-instance-region", defaults.NamespaceInstance.Region, "Namespace Compute region"),
		Endpoint:    fs.String("namespace-instance-endpoint", defaults.NamespaceInstance.Endpoint, "Namespace Compute API endpoint for nsc"),
		Keychain:    fs.String("namespace-instance-keychain", defaults.NamespaceInstance.Keychain, "Namespace keychain for nsc"),
		Volumes:     fs.String("namespace-instance-volume", strings.Join(defaults.NamespaceInstance.Volumes, ","), "Namespace Compute volume spec (comma-separated; passed through to nsc --volume)"),
		WorkRoot:    fs.String("namespace-instance-work-root", defaults.NamespaceInstance.WorkRoot, "remote Crabbox work root on Namespace Compute instances"),
	}
}

func ApplyProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "namespace-instance-machine-type") {
		cfg.NamespaceInstance.MachineType = *v.MachineType
	}
	if core.FlagWasSet(fs, "namespace-instance-duration") {
		duration, err := time.ParseDuration(*v.Duration)
		if err != nil || duration <= 0 {
			return core.Exit(2, "invalid --namespace-instance-duration %q", *v.Duration)
		}
		cfg.NamespaceInstance.Duration = duration
	}
	if core.FlagWasSet(fs, "namespace-instance-ephemeral") {
		cfg.NamespaceInstance.Ephemeral = *v.Ephemeral
	}
	if core.FlagWasSet(fs, "namespace-instance-region") {
		cfg.NamespaceInstance.Region = *v.Region
	}
	if core.FlagWasSet(fs, "namespace-instance-endpoint") {
		cfg.NamespaceInstance.Endpoint = *v.Endpoint
	}
	if core.FlagWasSet(fs, "namespace-instance-keychain") {
		cfg.NamespaceInstance.Keychain = *v.Keychain
	}
	if core.FlagWasSet(fs, "namespace-instance-volume") {
		cfg.NamespaceInstance.Volumes = splitCSV(*v.Volumes)
	}
	if core.FlagWasSet(fs, "namespace-instance-work-root") {
		cfg.NamespaceInstance.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	return validateNamespaceInstanceConfig(*cfg)
}

func splitCSV(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func namespaceInstanceMachineTypeForConfig(cfg core.Config) string {
	if strings.TrimSpace(cfg.NamespaceInstance.MachineType) != "" {
		return strings.TrimSpace(cfg.NamespaceInstance.MachineType)
	}
	if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
		return strings.TrimSpace(cfg.ServerType)
	}
	return namespaceInstanceMachineTypeForClass(cfg.Class)
}

func namespaceInstanceMachineTypeForClass(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "", "standard", "fast":
		return "4x8"
	case "large":
		return "8x16"
	case "beast":
		return "16x32"
	default:
		return strings.TrimSpace(class)
	}
}

func validateNamespaceInstanceConfig(cfg core.Config) error {
	return cleanNamespaceInstanceWorkRoot(namespaceInstanceWorkRoot(cfg))
}

func namespaceInstanceWorkRoot(cfg core.Config) string {
	return core.Blank(strings.TrimSpace(cfg.NamespaceInstance.WorkRoot), "/workspaces/crabbox")
}

func cleanNamespaceInstanceWorkRoot(workRoot string) error {
	clean := path.Clean(strings.TrimSpace(workRoot))
	if clean == "" || !strings.HasPrefix(clean, "/") {
		return core.Exit(2, "namespaceInstance.workRoot %q must resolve to an absolute path", workRoot)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspaces":
		return core.Exit(2, "namespaceInstance.workRoot %q is too broad; choose a dedicated subdirectory", clean)
	}
	return nil
}
