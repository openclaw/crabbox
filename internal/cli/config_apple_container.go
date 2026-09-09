package cli

import (
	"flag"
	"os"
	"strings"
)

// AppleContainerConfig is shared by the distinct Container and Machine flag surfaces.
type AppleContainerConfig struct {
	CLIPath      string   `yaml:"cliPath,omitempty"`
	Image        string   `yaml:"image,omitempty"`
	User         string   `yaml:"user,omitempty"`
	WorkRoot     string   `yaml:"workRoot,omitempty"`
	CPUs         int      `yaml:"cpus,omitempty"`
	Memory       string   `yaml:"memory,omitempty"`
	ExtraRunArgs []string `yaml:"extraRunArgs,omitempty"`
}

// Keep the named file destination and zero decoding separate from runtime defaults.
type fileAppleContainerConfig AppleContainerConfig

func initialAppleContainerConfig(image string) AppleContainerConfig {
	return AppleContainerConfig{
		CLIPath:  "container",
		Image:    image,
		User:     "crabbox",
		WorkRoot: "/work/crabbox",
	}
}

func (cfg *AppleContainerConfig) applyFile(file *fileAppleContainerConfig) (imageApplied bool) {
	if file == nil {
		return false
	}
	if file.CLIPath != "" {
		cfg.CLIPath = file.CLIPath
	}
	if file.Image != "" {
		cfg.Image = file.Image
		imageApplied = true
	}
	if file.User != "" {
		cfg.User = file.User
	}
	if file.WorkRoot != "" {
		cfg.WorkRoot = file.WorkRoot
	}
	if file.CPUs > 0 {
		cfg.CPUs = file.CPUs
	}
	if file.Memory != "" {
		cfg.Memory = file.Memory
	}
	if len(file.ExtraRunArgs) > 0 {
		cfg.ExtraRunArgs = append([]string(nil), file.ExtraRunArgs...)
	}
	return imageApplied
}

func (cfg *AppleContainerConfig) applyEnv() (imageApplied bool) {
	cfg.CLIPath = getenv("CRABBOX_APPLE_CONTAINER_CLI", cfg.CLIPath)
	if image := os.Getenv("CRABBOX_APPLE_CONTAINER_IMAGE"); image != "" {
		cfg.Image = image
		imageApplied = true
	}
	cfg.User = getenv("CRABBOX_APPLE_CONTAINER_USER", cfg.User)
	cfg.WorkRoot = getenv("CRABBOX_APPLE_CONTAINER_WORK_ROOT", cfg.WorkRoot)
	cfg.CPUs = getenvInt("CRABBOX_APPLE_CONTAINER_CPUS", cfg.CPUs)
	cfg.Memory = getenv("CRABBOX_APPLE_CONTAINER_MEMORY", cfg.Memory)
	if extra := strings.Fields(os.Getenv("CRABBOX_APPLE_CONTAINER_EXTRA_RUN_ARGS")); len(extra) > 0 {
		cfg.ExtraRunArgs = extra
	}
	return imageApplied
}

// AppleContainerConfigFlagValues holds the seven Container flags only.
type AppleContainerConfigFlagValues struct {
	CLIPath  *string
	Image    *string
	User     *string
	WorkRoot *string
	CPUs     *int
	Memory   *string
	ExtraRun *string
}

// AppleContainerConfigApplied reports accepted flag assignments for caller policy.
type AppleContainerConfigApplied struct {
	Image    bool
	User     bool
	WorkRoot bool
}

func RegisterAppleContainerConfigFlags(fs *flag.FlagSet, defaults AppleContainerConfig) AppleContainerConfigFlagValues {
	return AppleContainerConfigFlagValues{
		CLIPath:  fs.String("apple-container-cli", defaults.CLIPath, "path to Apple's container CLI"),
		Image:    fs.String("apple-container-image", defaults.Image, "container image for apple-container leases"),
		User:     fs.String("apple-container-user", defaults.User, "SSH user created inside apple-container leases"),
		WorkRoot: fs.String("apple-container-work-root", defaults.WorkRoot, "remote Crabbox work root inside apple-container leases"),
		CPUs:     fs.Int("apple-container-cpus", defaults.CPUs, "CPU limit for apple-container leases; 0 leaves runtime default"),
		Memory:   fs.String("apple-container-memory", defaults.Memory, "memory limit for apple-container leases, for example 8g"),
		ExtraRun: fs.String("apple-container-extra-run-args", strings.Join(defaults.ExtraRunArgs, " "), "extra arguments appended to container run, space separated"),
	}
}

func (values AppleContainerConfigFlagValues) Apply(cfg *AppleContainerConfig, fs *flag.FlagSet) AppleContainerConfigApplied {
	var applied AppleContainerConfigApplied
	if flagWasSet(fs, "apple-container-cli") {
		cfg.CLIPath = *values.CLIPath
	}
	if flagWasSet(fs, "apple-container-image") {
		cfg.Image = *values.Image
		applied.Image = true
	}
	if flagWasSet(fs, "apple-container-user") {
		cfg.User = *values.User
		applied.User = true
	}
	if flagWasSet(fs, "apple-container-work-root") {
		cfg.WorkRoot = *values.WorkRoot
		applied.WorkRoot = true
	}
	if flagWasSet(fs, "apple-container-cpus") {
		cfg.CPUs = *values.CPUs
	}
	if flagWasSet(fs, "apple-container-memory") {
		cfg.Memory = *values.Memory
	}
	if flagWasSet(fs, "apple-container-extra-run-args") {
		cfg.ExtraRunArgs = splitAppleContainerExtraArgs(*values.ExtraRun)
	}
	return applied
}

func splitAppleContainerExtraArgs(value string) []string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// AppleMachineConfigFlagValues holds Machine's separate four-flag surface.
type AppleMachineConfigFlagValues struct {
	CLIPath *string
	Image   *string
	CPUs    *int
	Memory  *string
}

func RegisterAppleMachineConfigFlags(fs *flag.FlagSet, defaults AppleContainerConfig) AppleMachineConfigFlagValues {
	return AppleMachineConfigFlagValues{
		CLIPath: fs.String("apple-machine-cli", defaults.CLIPath, "path to Apple's container CLI"),
		Image:   fs.String("apple-machine-image", defaults.Image, "OCI image for apple-machine leases"),
		CPUs:    fs.Int("apple-machine-cpus", defaults.CPUs, "CPU count for apple-machine leases"),
		Memory:  fs.String("apple-machine-memory", defaults.Memory, "memory for apple-machine leases, for example 8G"),
	}
}

func (values AppleMachineConfigFlagValues) Apply(cfg *AppleContainerConfig, fs *flag.FlagSet) (imageApplied bool) {
	if flagWasSet(fs, "apple-machine-cli") {
		cfg.CLIPath = *values.CLIPath
	}
	if flagWasSet(fs, "apple-machine-image") {
		cfg.Image = *values.Image
		imageApplied = true
	}
	if flagWasSet(fs, "apple-machine-cpus") {
		cfg.CPUs = *values.CPUs
	}
	if flagWasSet(fs, "apple-machine-memory") {
		cfg.Memory = *values.Memory
	}
	return imageApplied
}
