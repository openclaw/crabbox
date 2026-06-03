package localcontainer

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type volumeListFlag []string

func (f *volumeListFlag) String() string { return strings.Join(*f, ",") }
func (f *volumeListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type flagValues struct {
	Runtime      *string
	Image        *string
	User         *string
	WorkRoot     *string
	CPUs         *int
	Memory       *string
	Network      *string
	DockerSocket *bool
	Volumes      volumeListFlag
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	v := flagValues{
		Runtime:  fs.String("local-container-runtime", defaults.LocalContainer.Runtime, "Docker-compatible CLI to use for local containers"),
		Image:    fs.String("local-container-image", defaults.LocalContainer.Image, "container image for local-container leases"),
		User:     fs.String("local-container-user", defaults.LocalContainer.User, "SSH user created inside local-container leases"),
		WorkRoot: fs.String("local-container-work-root", defaults.LocalContainer.WorkRoot, "remote Crabbox work root inside local-container leases"),
		CPUs:     fs.Int("local-container-cpus", defaults.LocalContainer.CPUs, "CPU limit for local-container leases; 0 leaves runtime default"),
		Memory:   fs.String("local-container-memory", defaults.LocalContainer.Memory, "memory limit for local-container leases, for example 8g"),
		Network:  fs.String("local-container-network", defaults.LocalContainer.Network, "container network for local-container leases"),
		DockerSocket: fs.Bool("local-container-docker-socket", defaults.LocalContainer.DockerSocket,
			"mount /var/run/docker.sock into local-container leases so docker commands use the host daemon"),
	}
	v.Volumes = volumeListFlag(defaults.LocalContainer.Volumes)
	fs.Var(&v.Volumes, "local-container-volume",
		"bind-mount a host path into the container; host:container[:ro]; repeatable")
	return v
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "local-container-runtime") {
		cfg.LocalContainer.Runtime = *v.Runtime
	}
	if core.FlagWasSet(fs, "local-container-image") {
		cfg.LocalContainer.Image = *v.Image
		core.MarkLocalContainerImageExplicit(cfg)
	}
	if core.FlagWasSet(fs, "local-container-user") {
		cfg.LocalContainer.User = *v.User
		cfg.SSHUser = *v.User
	}
	if core.FlagWasSet(fs, "local-container-work-root") {
		cfg.LocalContainer.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if core.FlagWasSet(fs, "local-container-cpus") {
		cfg.LocalContainer.CPUs = *v.CPUs
	}
	if core.FlagWasSet(fs, "local-container-memory") {
		cfg.LocalContainer.Memory = *v.Memory
	}
	if core.FlagWasSet(fs, "local-container-network") {
		cfg.LocalContainer.Network = *v.Network
	}
	if core.FlagWasSet(fs, "local-container-docker-socket") {
		cfg.LocalContainer.DockerSocket = *v.DockerSocket
	}
	if core.FlagWasSet(fs, "local-container-volume") {
		cfg.LocalContainer.Volumes = []string(v.Volumes)
	}
	if cfg.Provider == providerName || cfg.Provider == "docker" || cfg.Provider == "container" || cfg.Provider == "local-docker" {
		applyDefaults(cfg)
	}
	return nil
}
