package cli

import (
	"flag"
	"os"
	"strconv"
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

func (cfg *AppleContainerConfig) applyFile(file *fileAppleContainerConfig) AppleContainerConfigApplied {
	var applied AppleContainerConfigApplied
	if file == nil {
		return applied
	}
	if file.CLIPath != "" {
		cfg.CLIPath = file.CLIPath
		applied.InputAccepted = true
	}
	if file.Image != "" {
		cfg.Image = file.Image
		applied.Image = true
		applied.InputAccepted = true
	}
	if file.User != "" {
		cfg.User = file.User
		applied.InputAccepted = true
	}
	if file.WorkRoot != "" {
		cfg.WorkRoot = file.WorkRoot
		applied.InputAccepted = true
	}
	if file.CPUs > 0 {
		cfg.CPUs = file.CPUs
		applied.InputAccepted = true
	}
	if file.Memory != "" {
		cfg.Memory = file.Memory
		applied.InputAccepted = true
	}
	if len(file.ExtraRunArgs) > 0 {
		cfg.ExtraRunArgs = append([]string(nil), file.ExtraRunArgs...)
		applied.InputAccepted = true
	}
	return applied
}

func (cfg *AppleContainerConfig) applyEnv() AppleContainerConfigApplied {
	var applied AppleContainerConfigApplied
	if value := os.Getenv("CRABBOX_APPLE_CONTAINER_CLI"); value != "" {
		cfg.CLIPath = value
		applied.InputAccepted = true
	}
	if image := os.Getenv("CRABBOX_APPLE_CONTAINER_IMAGE"); image != "" {
		cfg.Image = image
		applied.Image = true
		applied.InputAccepted = true
	}
	if value := os.Getenv("CRABBOX_APPLE_CONTAINER_USER"); value != "" {
		cfg.User = value
		applied.InputAccepted = true
	}
	if value := os.Getenv("CRABBOX_APPLE_CONTAINER_WORK_ROOT"); value != "" {
		cfg.WorkRoot = value
		applied.InputAccepted = true
	}
	if value, ok := lookupEnvInteger("CRABBOX_APPLE_CONTAINER_CPUS", strconv.IntSize); ok {
		cfg.CPUs = int(value)
		applied.InputAccepted = true
	}
	if value := os.Getenv("CRABBOX_APPLE_CONTAINER_MEMORY"); value != "" {
		cfg.Memory = value
		applied.InputAccepted = true
	}
	if extra := strings.Fields(os.Getenv("CRABBOX_APPLE_CONTAINER_EXTRA_RUN_ARGS")); len(extra) > 0 {
		cfg.ExtraRunArgs = extra
		applied.InputAccepted = true
	}
	return applied
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

// AppleContainerConfigApplied reports accepted input and selective caller policy.
type AppleContainerConfigApplied struct {
	InputAccepted bool
	Image         bool
	User          bool
	WorkRoot      bool
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
		applied.InputAccepted = true
	}
	if flagWasSet(fs, "apple-container-image") {
		cfg.Image = *values.Image
		applied.InputAccepted = true
		applied.Image = true
	}
	if flagWasSet(fs, "apple-container-user") {
		cfg.User = *values.User
		applied.InputAccepted = true
		applied.User = true
	}
	if flagWasSet(fs, "apple-container-work-root") {
		cfg.WorkRoot = *values.WorkRoot
		applied.InputAccepted = true
		applied.WorkRoot = true
	}
	if flagWasSet(fs, "apple-container-cpus") {
		cfg.CPUs = *values.CPUs
		applied.InputAccepted = true
	}
	if flagWasSet(fs, "apple-container-memory") {
		cfg.Memory = *values.Memory
		applied.InputAccepted = true
	}
	if flagWasSet(fs, "apple-container-extra-run-args") {
		cfg.ExtraRunArgs = splitAppleContainerExtraArgs(*values.ExtraRun)
		applied.InputAccepted = true
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

func (values AppleMachineConfigFlagValues) Apply(cfg *AppleContainerConfig, fs *flag.FlagSet) AppleContainerConfigApplied {
	var applied AppleContainerConfigApplied
	if flagWasSet(fs, "apple-machine-cli") {
		cfg.CLIPath = *values.CLIPath
		applied.InputAccepted = true
	}
	if flagWasSet(fs, "apple-machine-image") {
		cfg.Image = *values.Image
		applied.Image = true
		applied.InputAccepted = true
	}
	if flagWasSet(fs, "apple-machine-cpus") {
		cfg.CPUs = *values.CPUs
		applied.InputAccepted = true
	}
	if flagWasSet(fs, "apple-machine-memory") {
		cfg.Memory = *values.Memory
		applied.InputAccepted = true
	}
	return applied
}
