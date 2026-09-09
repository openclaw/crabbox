package cli

//go:generate go run ../../scripts/configgen -source config_exe_dev.go -output config_exe_dev_generated.go -type ExeDevConfig -provider exe-dev

// These values are runtime and presentation fallbacks, not configured defaults.
// Empty WorkRoot permits inheritance; empty Image omits the native image option.
const (
	ExeDevWorkRootFallback  = "/tmp/crabbox"
	ExeDevDefaultImageLabel = "default"
)

// ExeDevConfig describes mechanical bindings for the SSH lease provider.
// Control-host provenance and destination validation remain core policy.
type ExeDevConfig struct {
	ControlHost string `config:"controlHost" env:"CRABBOX_EXE_DEV_CONTROL_HOST" envAlias:"EXE_DEV_CONTROL_HOST" flag:"exe-dev-control-host" sources:"user,repo,env,flag" help:"exe.dev SSH API host" default:"exe.dev" fileIgnoreEmpty:"true" reportApplied:"true"`
	Image       string `config:"image" env:"CRABBOX_EXE_DEV_IMAGE" envAlias:"EXE_DEV_IMAGE" flag:"exe-dev-image" sources:"user,repo,env,flag" help:"exe.dev VM image" fileIgnoreEmpty:"true"`
	CPUs        int    `config:"cpus" env:"CRABBOX_EXE_DEV_CPUS" flag:"exe-dev-cpus" sources:"user,repo,env,flag" help:"exe.dev VM CPUs" default:"2" nonnegative:"true" fileInt:"positive" envInt:"fallback"`
	Memory      string `config:"memory" env:"CRABBOX_EXE_DEV_MEMORY" envAlias:"EXE_DEV_MEMORY" flag:"exe-dev-memory" sources:"user,repo,env,flag" help:"exe.dev VM memory, for example 4GB" default:"4GB" fileIgnoreEmpty:"true"`
	Disk        string `config:"disk" env:"CRABBOX_EXE_DEV_DISK" envAlias:"EXE_DEV_DISK" flag:"exe-dev-disk" sources:"user,repo,env,flag" help:"exe.dev VM disk, for example 10GB" default:"10GB" fileIgnoreEmpty:"true"`
	Command     string `config:"command" env:"CRABBOX_EXE_DEV_COMMAND" flag:"exe-dev-command" sources:"user,repo,env,flag" help:"exe.dev container command" fileIgnoreEmpty:"true"`
	User        string `config:"user" env:"CRABBOX_EXE_DEV_USER" flag:"exe-dev-user" sources:"user,repo,env,flag" help:"SSH user for exe.dev VMs" fileIgnoreEmpty:"true"`
	WorkRoot    string `config:"workRoot" env:"CRABBOX_EXE_DEV_WORK_ROOT" flag:"exe-dev-work-root" sources:"user,repo,env,flag" help:"remote Crabbox work root on exe.dev VMs" fileIgnoreEmpty:"true"`
	NoEmail     bool   `config:"noEmail" env:"CRABBOX_EXE_DEV_NO_EMAIL" flag:"exe-dev-no-email" sources:"user,repo,env,flag" help:"suppress exe.dev VM notification email" default:"true"`
}
