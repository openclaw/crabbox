package cli

//go:generate go run ../../scripts/configgen -source config_lume.go -output config_lume_generated.go -type LumeConfig -provider lume

// LumeConfig describes mechanical bindings; host storage and user-derived
// work-root resolution remain owned by the provider.
type LumeConfig struct {
	CLIPath  string `config:"cliPath" env:"CRABBOX_LUME_CLI" flag:"lume-cli" sources:"user,env,flag" help:"path to the Lume CLI" default:"lume" fileIgnoreEmpty:"true"`
	Base     string `config:"base" env:"CRABBOX_LUME_BASE" flag:"lume-base" sources:"user,env,flag" help:"stopped Lume VM to clone for each lease" default:"crabbox-macos-golden" fileIgnoreEmpty:"true"`
	Storage  string `config:"storage" env:"CRABBOX_LUME_STORAGE" flag:"lume-storage" sources:"user,env,flag" help:"optional Lume storage location" fileIgnoreEmpty:"true"`
	User     string `config:"user" env:"CRABBOX_LUME_USER" flag:"lume-user" sources:"user,env,flag" help:"guest account prepared for SSH" default:"lume" fileIgnoreEmpty:"true"`
	WorkRoot string `config:"workRoot" env:"CRABBOX_LUME_WORK_ROOT" flag:"lume-work-root" sources:"user,repo,env,flag" help:"guest work root" default:"/Users/lume/crabbox" fileIgnoreEmpty:"true"`
}
