package cli

import "time"

//go:generate go run ../../scripts/configgen -source config_multipass.go -output config_multipass_generated.go -type MultipassConfig -provider multipass

// MultipassConfig owns input bindings; portable image selection stays with core.
type MultipassConfig struct {
	CLIPath       string        `config:"cliPath" env:"CRABBOX_MULTIPASS_CLI" flag:"multipass-cli" sources:"user,repo,env,flag" help:"Multipass CLI path" default:"multipass" fileIgnoreEmpty:"true" fileStorage:"value"`
	Image         string        `config:"image" env:"CRABBOX_MULTIPASS_IMAGE" flag:"multipass-image" sources:"user,repo,env,flag" help:"Multipass Ubuntu image selector" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	User          string        `config:"user" env:"CRABBOX_MULTIPASS_USER" flag:"multipass-user" sources:"user,repo,env,flag" help:"SSH user created inside Multipass leases" default:"crabbox" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	WorkRoot      string        `config:"workRoot" env:"CRABBOX_MULTIPASS_WORK_ROOT" flag:"multipass-work-root" sources:"user,repo,env,flag" help:"remote Crabbox work root inside Multipass leases" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	CPUs          int           `config:"cpus" env:"CRABBOX_MULTIPASS_CPUS" flag:"multipass-cpus" sources:"user,repo,env,flag" help:"CPU count for Multipass leases; 0 leaves Multipass default" default:"4" nonnegative:"true" fileInt:"positive" envInt:"fallback" fileStorage:"value"`
	Memory        string        `config:"memory" env:"CRABBOX_MULTIPASS_MEMORY" flag:"multipass-memory" sources:"user,repo,env,flag" help:"memory size for Multipass leases, for example 8G" default:"8G" fileIgnoreEmpty:"true" fileStorage:"value"`
	Disk          string        `config:"disk" env:"CRABBOX_MULTIPASS_DISK" flag:"multipass-disk" sources:"user,repo,env,flag" help:"disk size for Multipass leases, for example 30G" default:"30G" fileIgnoreEmpty:"true" fileStorage:"value"`
	LaunchTimeout time.Duration `config:"launchTimeout" env:"CRABBOX_MULTIPASS_LAUNCH_TIMEOUT" flag:"multipass-launch-timeout" sources:"user,repo,env,flag" help:"Multipass launch timeout" default:"20m" duration:"positive-overlay" fileStorage:"value" flagDuration:"raw-positive"`
}

// Keep the existing shared POSIX default, rather than introducing a separate literal.
const MultipassConfigDefaultWorkRoot = defaultPOSIXWorkRoot

func initialMultipassConfig(resolvedImage string) MultipassConfig {
	cfg := defaultMultipassConfig()
	cfg.Image = resolvedImage
	cfg.WorkRoot = MultipassConfigDefaultWorkRoot
	return cfg
}
