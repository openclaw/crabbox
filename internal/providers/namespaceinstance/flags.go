package namespaceinstance

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ",") }
func (f *stringListFlag) Set(value string) error {
	*f = append(*f, strings.TrimSpace(value))
	return nil
}

type flagValues struct {
	CLIPath     *string
	MachineType *string
	Duration    *string
	Region      *string
	Endpoint    *string
	Keychain    *string
	Volumes     *stringListFlag
	WorkRoot    *string
	Bare        *bool
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	volumes := stringListFlag(append([]string(nil), defaults.NamespaceInstance.Volumes...))
	values := flagValues{
		CLIPath:     fs.String("namespace-instance-cli", defaults.NamespaceInstance.CLIPath, "Namespace nsc CLI path"),
		MachineType: fs.String("namespace-instance-machine-type", defaults.NamespaceInstance.MachineType, "Namespace instance CPUxMemoryGB shape, for example 4x8"),
		Duration:    fs.String("namespace-instance-duration", defaults.NamespaceInstance.Duration.String(), "Namespace instance duration; defaults to --ttl"),
		Region:      fs.String("namespace-instance-region", defaults.NamespaceInstance.Region, "Namespace region"),
		Endpoint:    fs.String("namespace-instance-endpoint", defaults.NamespaceInstance.Endpoint, "Namespace API endpoint"),
		Keychain:    fs.String("namespace-instance-keychain", defaults.NamespaceInstance.Keychain, "Namespace token keychain"),
		Volumes:     &volumes,
		WorkRoot:    fs.String("namespace-instance-work-root", defaults.NamespaceInstance.WorkRoot, "remote Crabbox work root"),
		Bare:        fs.Bool("namespace-instance-bare", defaults.NamespaceInstance.Bare, "disable Kubernetes for a smaller instance"),
	}
	fs.Var(&volumes, "namespace-instance-volume", "Namespace volume kind:tag:mountpoint:size; repeatable")
	return values
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "namespace-instance-cli") {
		cfg.NamespaceInstance.CLIPath = *v.CLIPath
	}
	if core.FlagWasSet(fs, "namespace-instance-machine-type") {
		cfg.NamespaceInstance.MachineType = *v.MachineType
	}
	if core.FlagWasSet(fs, "namespace-instance-duration") {
		if strings.TrimSpace(*v.Duration) == "0s" {
			cfg.NamespaceInstance.Duration = 0
		} else if err := core.ApplyLeaseDuration(&cfg.NamespaceInstance.Duration, *v.Duration); err != nil {
			return err
		}
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
		cfg.NamespaceInstance.Volumes = append([]string(nil), (*v.Volumes)...)
	}
	if core.FlagWasSet(fs, "namespace-instance-work-root") {
		cfg.NamespaceInstance.WorkRoot = *v.WorkRoot
	}
	if core.FlagWasSet(fs, "namespace-instance-bare") {
		cfg.NamespaceInstance.Bare = *v.Bare
	}
	if isProviderName(cfg.Provider) {
		applyDefaults(cfg)
	}
	return nil
}

func isProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "namespace-compute":
		return true
	default:
		return false
	}
}
