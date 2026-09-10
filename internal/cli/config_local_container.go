package cli

import "os"

type LocalContainerConfig struct {
	Runtime            string
	Image              string
	User               string
	WorkRoot           string
	CPUs               int
	Memory             string
	Network            string
	DockerSocket       bool
	NoHostname         bool
	Volumes            []string
	CheckpointMetadata map[string]string `yaml:"-" json:"-"`
}

type fileLocalContainerConfig struct {
	Runtime      string `yaml:"runtime,omitempty"`
	Image        string `yaml:"image,omitempty"`
	User         string `yaml:"user,omitempty"`
	WorkRoot     string `yaml:"workRoot,omitempty"`
	CPUs         int    `yaml:"cpus,omitempty"`
	Memory       string `yaml:"memory,omitempty"`
	Network      string `yaml:"network,omitempty"`
	DockerSocket *bool  `yaml:"dockerSocket,omitempty"`
	NoHostname   *bool  `yaml:"noHostname,omitempty"`
}

func initialLocalContainerConfig(resolvedImage string) LocalContainerConfig {
	return LocalContainerConfig{
		Runtime: "docker",
		Image:   resolvedImage,
		User:    "crabbox",
		Network: "bridge",
	}
}

func applyLocalContainerFile(cfg *Config, file *fileLocalContainerConfig) {
	if file == nil {
		return
	}
	if file.Runtime != "" {
		ApplyLocalContainerRuntime(cfg, file.Runtime)
	}
	if file.Image != "" {
		ApplyLocalContainerImage(cfg, file.Image)
	}
	if file.User != "" {
		cfg.LocalContainer.User = file.User
	}
	if file.WorkRoot != "" {
		ApplyLocalContainerWorkRoot(cfg, file.WorkRoot)
	}
	if file.CPUs > 0 {
		cfg.LocalContainer.CPUs = file.CPUs
	}
	if file.Memory != "" {
		cfg.LocalContainer.Memory = file.Memory
	}
	if file.Network != "" {
		cfg.LocalContainer.Network = file.Network
	}
	applyOptional(&cfg.LocalContainer.DockerSocket, file.DockerSocket)
	applyOptional(&cfg.LocalContainer.NoHostname, file.NoHostname)
	// NOTE: localContainer.volumes is intentionally NOT loaded from
	// repo-local config files. Bind mounts expose host paths and must
	// be an explicit CLI action (--local-container-volume), not
	// something an untrusted checkout can request via .crabbox.yaml.
}

func applyLocalContainerEnv(cfg *Config) {
	if runtimeName := os.Getenv("CRABBOX_LOCAL_CONTAINER_RUNTIME"); runtimeName != "" {
		ApplyLocalContainerRuntime(cfg, runtimeName)
	}
	if image := os.Getenv("CRABBOX_LOCAL_CONTAINER_IMAGE"); image != "" {
		ApplyLocalContainerImage(cfg, image)
	}
	cfg.LocalContainer.User = getenv("CRABBOX_LOCAL_CONTAINER_USER", cfg.LocalContainer.User)
	if workRoot := os.Getenv("CRABBOX_LOCAL_CONTAINER_WORK_ROOT"); workRoot != "" {
		ApplyLocalContainerWorkRoot(cfg, workRoot)
	}
	cfg.LocalContainer.CPUs = getenvInt("CRABBOX_LOCAL_CONTAINER_CPUS", cfg.LocalContainer.CPUs)
	cfg.LocalContainer.Memory = getenv("CRABBOX_LOCAL_CONTAINER_MEMORY", cfg.LocalContainer.Memory)
	cfg.LocalContainer.Network = getenv("CRABBOX_LOCAL_CONTAINER_NETWORK", cfg.LocalContainer.Network)
	if value, ok := getenvBool("CRABBOX_LOCAL_CONTAINER_DOCKER_SOCKET"); ok {
		cfg.LocalContainer.DockerSocket = value
	}
	if value, ok := getenvBool("CRABBOX_LOCAL_CONTAINER_NO_HOSTNAME"); ok {
		cfg.LocalContainer.NoHostname = value
	}
}

// ApplyLocalContainerRuntime applies an already-accepted runtime value and sets
// the existing marker via MarkLocalContainerRuntimeExplicit.
func ApplyLocalContainerRuntime(cfg *Config, value string) {
	cfg.LocalContainer.Runtime = value
	MarkLocalContainerRuntimeExplicit(cfg)
}

// ApplyLocalContainerImage applies an already-accepted image value and sets
// the existing marker via MarkLocalContainerImageExplicit.
func ApplyLocalContainerImage(cfg *Config, value string) {
	cfg.LocalContainer.Image = value
	MarkLocalContainerImageExplicit(cfg)
}

// ApplyLocalContainerWorkRoot applies an already-accepted work-root value and sets
// the existing marker via MarkLocalContainerWorkRootExplicit.
func ApplyLocalContainerWorkRoot(cfg *Config, value string) {
	cfg.LocalContainer.WorkRoot = value
	MarkLocalContainerWorkRootExplicit(cfg)
}
