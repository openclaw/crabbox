package namespaceinstance

import (
	"flag"
	"path"
	"strings"
	"time"
)

type flagValues struct {
	MachineType *string
	Duration    *string
	Ephemeral   *bool
	Region      *string
	Endpoint    *string
	Keychain    *string
	WorkRoot    *string
	Volume      *string
}

func RegisterNamespaceInstanceProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return flagValues{
		MachineType: fs.String("namespace-instance-machine-type", defaults.NamespaceInstance.MachineType, "Namespace Instance machine type"),
		Duration:    fs.String("namespace-instance-duration", durationString(defaults.NamespaceInstance.Duration), "Namespace Instance duration"),
		Ephemeral:   fs.Bool("namespace-instance-ephemeral", defaults.NamespaceInstance.Ephemeral, "request ephemeral Namespace Instances"),
		Region:      fs.String("namespace-instance-region", defaults.NamespaceInstance.Region, "Namespace Instance region"),
		Endpoint:    fs.String("namespace-instance-endpoint", defaults.NamespaceInstance.Endpoint, "Namespace nsc API endpoint"),
		Keychain:    fs.String("namespace-instance-keychain", defaults.NamespaceInstance.Keychain, "Namespace nsc keychain name"),
		WorkRoot:    fs.String("namespace-instance-work-root", defaults.NamespaceInstance.WorkRoot, "remote Crabbox work root on Namespace Instances"),
		Volume:      fs.String("namespace-instance-volume", strings.Join(defaults.NamespaceInstance.Volumes, ","), "comma-separated Namespace Instance volume specs"),
	}
}

func ApplyNamespaceInstanceProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "namespace-instance-machine-type") {
		cfg.NamespaceInstance.MachineType = strings.TrimSpace(*v.MachineType)
		cfg.ServerType = cfg.NamespaceInstance.MachineType
		cfg.ServerTypeExplicit = true
	}
	if flagWasSet(fs, "namespace-instance-duration") {
		duration, err := parsePositiveDuration(*v.Duration, "namespace-instance duration")
		if err != nil {
			return err
		}
		cfg.NamespaceInstance.Duration = duration
	}
	if flagWasSet(fs, "namespace-instance-ephemeral") {
		cfg.NamespaceInstance.Ephemeral = *v.Ephemeral
	}
	if flagWasSet(fs, "namespace-instance-region") {
		cfg.NamespaceInstance.Region = strings.TrimSpace(*v.Region)
	}
	if flagWasSet(fs, "namespace-instance-endpoint") {
		cfg.NamespaceInstance.Endpoint = strings.TrimSpace(*v.Endpoint)
	}
	if flagWasSet(fs, "namespace-instance-keychain") {
		cfg.NamespaceInstance.Keychain = strings.TrimSpace(*v.Keychain)
	}
	if flagWasSet(fs, "namespace-instance-work-root") {
		cfg.NamespaceInstance.WorkRoot = strings.TrimSpace(*v.WorkRoot)
		cfg.WorkRoot = cfg.NamespaceInstance.WorkRoot
	}
	if flagWasSet(fs, "namespace-instance-volume") {
		cfg.NamespaceInstance.Volumes = splitVolumeList(*v.Volume)
	}
	applyNamespaceInstanceDefaults(cfg)
	return validateNamespaceInstanceConfig(*cfg)
}

func applyNamespaceInstanceDefaults(cfg *Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetLinux
	}
	if cfg.NamespaceInstance.WorkRoot == "" {
		cfg.NamespaceInstance.WorkRoot = defaultWorkRoot
	}
	cfg.WorkRoot = cfg.NamespaceInstance.WorkRoot
	if cfg.NamespaceInstance.Duration == 0 && cfg.TTL > 0 {
		cfg.NamespaceInstance.Duration = cfg.TTL
	}
	if cfg.ServerType == "" || !cfg.ServerTypeExplicit {
		cfg.ServerType = namespaceInstanceServerTypeForConfig(*cfg)
	}
}

func validateNamespaceInstanceConfig(cfg Config) error {
	if strings.TrimSpace(cfg.TargetOS) != "" && strings.TrimSpace(cfg.TargetOS) != targetLinux {
		return exit(2, "provider=%s supports target=linux only, got target=%s", providerName, cfg.TargetOS)
	}
	if cfg.NamespaceInstance.Duration < 0 {
		return exit(2, "namespace-instance duration must be positive")
	}
	if err := validateWorkRoot(cfg.NamespaceInstance.WorkRoot); err != nil {
		return err
	}
	return nil
}

func namespaceInstanceServerTypeForConfig(cfg Config) string {
	if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
		return strings.TrimSpace(cfg.ServerType)
	}
	if value := strings.TrimSpace(cfg.NamespaceInstance.MachineType); value != "" {
		return value
	}
	return namespaceInstanceServerTypeForClass(cfg.Class)
}

func namespaceInstanceServerTypeForClass(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "", "standard", "fast", "large", "beast":
		return defaultMachineType
	default:
		return defaultMachineType
	}
}

func parsePositiveDuration(value, field string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, exit(2, "%s must be a positive duration", field)
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, exit(2, "%s must be a positive duration", field)
	}
	return parsed, nil
}

func durationString(value time.Duration) string {
	if value == 0 {
		return ""
	}
	return value.String()
}

func splitVolumeList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func validateWorkRoot(workRoot string) error {
	clean := path.Clean(strings.TrimSpace(workRoot))
	if clean == "" || !strings.HasPrefix(clean, "/") {
		return exit(2, "namespaceInstance.workRoot %q must resolve to an absolute path", workRoot)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/work", "/workspace", "/workspaces":
		return exit(2, "namespaceInstance.workRoot %q is too broad; choose a dedicated subdirectory", clean)
	}
	return nil
}
