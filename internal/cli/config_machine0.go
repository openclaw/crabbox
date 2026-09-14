package cli

import "time"

//go:generate go run ../../scripts/configgen -source config_machine0.go -output config_machine0_generated.go -type Machine0Config -provider machine0

// Machine0Config owns input bindings; runtime selection and validation stay provider-owned.
type Machine0Config struct {
	CLIPath       string        `config:"cliPath" env:"CRABBOX_MACHINE0_CLI" flag:"machine0-cli" sources:"user,repo,env,flag" help:"Machine0 CLI path" default:"machine0" fileIgnoreEmpty:"true" fileStorage:"value"`
	Image         string        `config:"image" env:"CRABBOX_MACHINE0_IMAGE" flag:"machine0-image" sources:"user,repo,env,flag" help:"Machine0 image name" default:"ubuntu-24-04-loaded" fileIgnoreEmpty:"true" fileStorage:"value"`
	ImageVersion  int           `config:"imageVersion" env:"CRABBOX_MACHINE0_IMAGE_VERSION" flag:"machine0-image-version" sources:"user,repo,env,flag" help:"Machine0 image version; 0 uses the active version" nonnegative:"true" fileInt:"present" envInt:"fallback"`
	DesktopImage  string        `config:"desktopImage" env:"CRABBOX_MACHINE0_DESKTOP_IMAGE" flag:"machine0-desktop-image" sources:"user,repo,env,flag" help:"optional prepared Machine0 image for --desktop leases" fileIgnoreEmpty:"true" fileStorage:"value"`
	Size          string        `config:"size" env:"CRABBOX_MACHINE0_SIZE" flag:"machine0-size" sources:"user,repo,env,flag" help:"Machine0 live-catalog size slug" default:"large" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	SizeExplicit  bool          `sources:"runtime"`
	Region        string        `config:"region" env:"CRABBOX_MACHINE0_REGION" flag:"machine0-region" sources:"user,repo,env,flag" help:"Machine0 region" default:"eu" fileIgnoreEmpty:"true" fileStorage:"value"`
	Key           string        `config:"key" env:"CRABBOX_MACHINE0_KEY" flag:"machine0-key" sources:"user,repo,env,flag" help:"Machine0 registered SSH key name; empty uses the Machine0 default" fileIgnoreEmpty:"true" fileStorage:"value"`
	WorkRoot      string        `config:"workRoot" env:"CRABBOX_MACHINE0_WORK_ROOT" flag:"machine0-work-root" sources:"user,repo,env,flag" help:"remote Crabbox work root" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	ReleasePolicy string        `config:"releasePolicy" env:"CRABBOX_MACHINE0_RELEASE_POLICY" flag:"machine0-release-policy" sources:"user,repo,env,flag" help:"release policy: destroy or explicit suspend" default:"destroy" fileIgnoreEmpty:"true" fileStorage:"value"`
	CreateTimeout time.Duration `config:"createTimeout" env:"CRABBOX_MACHINE0_CREATE_TIMEOUT" flag:"machine0-create-timeout" sources:"user,repo,env,flag" help:"Machine0 creation timeout" default:"15m" duration:"positive-overlay" fileStorage:"value" flagDuration:"raw-positive"`
	PollInterval  time.Duration `config:"pollInterval" env:"CRABBOX_MACHINE0_POLL_INTERVAL" flag:"machine0-poll-interval" sources:"user,repo,env,flag" help:"Machine0 status polling interval" default:"60s" duration:"positive-overlay" fileStorage:"value" flagDuration:"raw-positive"`
}
