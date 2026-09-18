package cli

//go:generate go run ../../scripts/configgen -source config_local_container.go -output config_local_container_generated.go -type LocalContainerConfig -provider local-container

// LocalContainerConfig owns bindings; creation checks and runtime defaults stay provider-owned.
//
//configgen:flag-application manual
type LocalContainerConfig struct {
	Runtime      string `config:"runtime" env:"CRABBOX_LOCAL_CONTAINER_RUNTIME" flag:"local-container-runtime" sources:"user,repo,env,flag" help:"Docker-compatible CLI to use for local containers" default:"docker" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Image        string `config:"image" env:"CRABBOX_LOCAL_CONTAINER_IMAGE" flag:"local-container-image" sources:"user,repo,env,flag" help:"container image for local-container leases" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	User         string `config:"user" env:"CRABBOX_LOCAL_CONTAINER_USER" flag:"local-container-user" sources:"user,repo,env,flag" help:"SSH user created inside local-container leases" default:"crabbox" fileIgnoreEmpty:"true" fileStorage:"value"`
	WorkRoot     string `config:"workRoot" env:"CRABBOX_LOCAL_CONTAINER_WORK_ROOT" flag:"local-container-work-root" sources:"user,repo,env,flag" help:"remote Crabbox work root inside local-container leases" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	CPUs         int    `config:"cpus" env:"CRABBOX_LOCAL_CONTAINER_CPUS" flag:"local-container-cpus" sources:"user,repo,env,flag" help:"CPU limit for local-container leases; 0 leaves runtime default" nonnegative:"true" fileInt:"positive" fileStorage:"value" envInt:"fallback"`
	Memory       string `config:"memory" env:"CRABBOX_LOCAL_CONTAINER_MEMORY" flag:"local-container-memory" sources:"user,repo,env,flag" help:"memory limit for local-container leases, for example 8g" fileIgnoreEmpty:"true" fileStorage:"value"`
	Network      string `config:"network" env:"CRABBOX_LOCAL_CONTAINER_NETWORK" flag:"local-container-network" sources:"user,repo,env,flag" help:"container network for local-container leases" default:"bridge" fileIgnoreEmpty:"true" fileStorage:"value"`
	DockerSocket bool   `config:"dockerSocket" env:"CRABBOX_LOCAL_CONTAINER_DOCKER_SOCKET" flag:"local-container-docker-socket" sources:"user,repo,env,flag" help:"mount the host Docker-compatible socket into local-container leases so docker commands use the host engine"`
	NoHostname   bool   `config:"noHostname" env:"CRABBOX_LOCAL_CONTAINER_NO_HOSTNAME" sources:"user,repo,env"`
	// Host volumes remain an explicit CLI action, never file or environment input.
	Volumes            []string          `flag:"local-container-volume" sources:"flag" help:"bind-mount a host path into the container; host:container[:ro]; repeatable" flagList:"append-raw"`
	CheckpointMetadata map[string]string `sources:"runtime" yaml:"-" json:"-"`
}

func initialLocalContainerConfig(resolvedImage string) LocalContainerConfig {
	cfg := defaultLocalContainerConfig()
	cfg.Image = resolvedImage
	return cfg
}

func applyLocalContainerFile(cfg *Config, file *fileLocalContainerConfig) (LocalContainerConfigApplied, error) {
	applied, err := cfg.LocalContainer.applyFile(file)
	markLocalContainerAcceptedConfig(cfg, applied)
	return applied, err
}

func applyLocalContainerEnv(cfg *Config) (LocalContainerConfigApplied, error) {
	applied, err := cfg.LocalContainer.applyEnv()
	markLocalContainerAcceptedConfig(cfg, applied)
	return applied, err
}

func markLocalContainerAcceptedConfig(cfg *Config, applied LocalContainerConfigApplied) {
	if applied.Runtime {
		MarkLocalContainerRuntimeExplicit(cfg)
	}
	if applied.Image {
		MarkLocalContainerImageExplicit(cfg)
	}
	if applied.WorkRoot {
		MarkLocalContainerWorkRootExplicit(cfg)
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
