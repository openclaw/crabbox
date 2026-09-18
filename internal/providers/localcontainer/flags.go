package localcontainer

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterLocalContainerConfigFlags(fs, defaults.LocalContainer)
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.LocalContainerConfigFlagValues)
	if !ok {
		return nil
	}
	visited := core.LocalContainerConfigFlagPresence(fs)
	if visited.Runtime {
		core.ApplyLocalContainerRuntime(cfg, *v.Runtime)
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.Image {
		core.ApplyLocalContainerImage(cfg, *v.Image)
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.User {
		cfg.LocalContainer.User = *v.User
		cfg.SSHUser = *v.User
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.WorkRoot {
		core.ApplyLocalContainerWorkRoot(cfg, *v.WorkRoot)
		cfg.WorkRoot = *v.WorkRoot
		core.MarkWorkRootExplicit(cfg)
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.CPUs {
		cfg.LocalContainer.CPUs = *v.CPUs
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.Memory {
		cfg.LocalContainer.Memory = *v.Memory
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.Network {
		cfg.LocalContainer.Network = *v.Network
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.DockerSocket {
		cfg.LocalContainer.DockerSocket = *v.DockerSocket
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if v.Volumes != nil && len(*v.Volumes) > 0 {
		if idFlag := fs.Lookup("id"); idFlag != nil && strings.TrimSpace(idFlag.Value.String()) != "" {
			return core.Exit(2, "--local-container-volume only applies when creating a new lease; omit --id or warm a new lease")
		}
		if poolFlag := fs.Lookup("pool"); poolFlag != nil && strings.TrimSpace(poolFlag.Value.String()) != "" {
			return core.Exit(2, "--local-container-volume only applies when creating a new lease; omit --pool or warm a new lease")
		}
		cfg.LocalContainer.Volumes = *v.Volumes
		core.RecordProviderFlagInputs(cfg, visited.Volumes, providerName)
	}
	if core.ProviderNameMatchesExact(cfg.Provider, Provider{}) {
		applyDefaults(cfg)
	}
	return nil
}
