package cli

//go:generate go run ../../scripts/configgen -source config_morph.go -output config_morph_generated.go -type MorphConfig -provider morph

// MorphConfig describes mechanical bindings for the SSH lease provider.
// Credential source and explicit release-policy markers remain handwritten.
type MorphConfig struct {
	APIKey          string `config:"apiKey" env:"CRABBOX_MORPH_API_KEY" envAlias:"MORPH_API_KEY" sources:"user,repo,env" fileIgnoreEmpty:"true" reportApplied:"true" fileStorage:"value"`
	APIURL          string `config:"apiUrl" env:"CRABBOX_MORPH_API_URL" flag:"morph-api-url" sources:"user,repo,env,flag" help:"Morph API URL" default:"https://cloud.morph.so" fileIgnoreEmpty:"true" reportApplied:"true" fileStorage:"value"`
	Snapshot        string `config:"snapshot" env:"CRABBOX_MORPH_SNAPSHOT" flag:"morph-snapshot" sources:"user,repo,env,flag" help:"Morph snapshot ID" fileIgnoreEmpty:"true" fileStorage:"value"`
	SSHGatewayHost  string `config:"sshGatewayHost" env:"CRABBOX_MORPH_SSH_GATEWAY_HOST" flag:"morph-ssh-gateway-host" sources:"user,repo,env,flag" help:"Morph SSH gateway host" default:"ssh.cloud.morph.so" fileIgnoreEmpty:"true" reportApplied:"true" fileStorage:"value"`
	WorkRoot        string `config:"workRoot" env:"CRABBOX_MORPH_WORK_ROOT" flag:"morph-work-root" sources:"user,repo,env,flag" help:"Morph remote Crabbox work root" default:"/tmp/crabbox" fileIgnoreEmpty:"true" fileStorage:"value"`
	DeleteOnRelease bool   `config:"deleteOnRelease" env:"CRABBOX_MORPH_DELETE_ON_RELEASE" flag:"morph-delete-on-release" sources:"user,repo,env,flag" help:"Delete Morph instances instead of pausing them on release" reportApplied:"true"`
	WakeOnSSH       bool   `config:"wakeOnSSH" env:"CRABBOX_MORPH_WAKE_ON_SSH" flag:"morph-wake-on-ssh" sources:"user,repo,env,flag" help:"Enable Morph wake-on-ssh for paused instances" default:"true"`
}
