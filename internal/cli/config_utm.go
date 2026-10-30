package cli

import "time"

//go:generate go run ../../scripts/configgen -source config_utm.go -output config_utm_generated.go -type UTMConfig -provider utm

// UTMConfig selects prepared Linux QEMU clones or fresh Apple macOS installs.
// UTM control uses the local macOS application context.
type UTMConfig struct {
	Acquisition    string        `config:"acquisition" env:"CRABBOX_UTM_ACQUISITION" flag:"utm-acquisition" sources:"user,repo,env,flag" help:"UTM acquisition: template or ipsw" default:"template" fileIgnoreEmpty:"true" fileStorage:"value"`
	IPSW           string        `config:"ipsw" env:"CRABBOX_UTM_IPSW" flag:"utm-ipsw" sources:"user,repo,env,flag" help:"local macOS 27+ IPSW for fresh Apple acquisition (requires full Xcode)" fileIgnoreEmpty:"true" fileStorage:"value"`
	InstallTimeout time.Duration `config:"installTimeout" env:"CRABBOX_UTM_INSTALL_TIMEOUT" flag:"utm-install-timeout" sources:"user,repo,env,flag" help:"bounded macOS installation timeout" default:"45m" duration:"positive-overlay" fileStorage:"value" flagDuration:"raw-positive"`

	AppPath        string        `config:"appPath" env:"CRABBOX_UTM_APP" flag:"utm-app" sources:"user,repo,env,flag" help:"local UTM application bundle" default:"/Applications/UTM.app" fileIgnoreEmpty:"true" fileStorage:"value"`
	StorageRoot    string        `config:"storageRoot" env:"CRABBOX_UTM_STORAGE_ROOT" flag:"utm-storage-root" sources:"user,repo,env,flag" help:"local directory containing UTM cloned .utm bundles" fileIgnoreEmpty:"true" fileStorage:"value"`
	Template       string        `config:"template" env:"CRABBOX_UTM_TEMPLATE" flag:"utm-template" sources:"user,repo,env,flag" help:"stopped Linux QEMU template UUID" fileIgnoreEmpty:"true" fileStorage:"value"`
	User           string        `config:"user" env:"CRABBOX_UTM_USER" flag:"utm-user" sources:"user,repo,env,flag" help:"prepared guest SSH user" default:"crabbox" fileIgnoreEmpty:"true" fileStorage:"value"`
	SSHKey         string        `config:"sshKey" env:"CRABBOX_UTM_SSH_KEY" flag:"utm-ssh-key" sources:"user,repo,env,flag" help:"private SSH key already authorized in the template" fileIgnoreEmpty:"true" fileStorage:"value"`
	WorkRoot       string        `config:"workRoot" env:"CRABBOX_UTM_WORK_ROOT" flag:"utm-work-root" sources:"user,repo,env,flag" help:"writable guest work root" default:"/tmp/crabbox" fileIgnoreEmpty:"true" fileStorage:"value"`
	StartupTimeout time.Duration `config:"startupTimeout" env:"CRABBOX_UTM_STARTUP_TIMEOUT" flag:"utm-startup-timeout" sources:"user,repo,env,flag" help:"bounded UTM boot and authenticated SSH readiness timeout" default:"5m" duration:"positive-overlay" fileStorage:"value" flagDuration:"raw-positive"`
}
