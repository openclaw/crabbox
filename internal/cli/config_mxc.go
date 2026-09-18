package cli

//go:generate go run ../../scripts/configgen -source config_mxc.go -output config_mxc_generated.go -type MXCConfig -provider mxc

// MXCConfig owns inert configuration bindings; execution policy stays in the adapter.
type MXCConfig struct {
	CLIPath           string   `config:"cliPath" env:"CRABBOX_MXC_CLI" flag:"mxc-cli" sources:"user,repo,env,flag" help:"path to the MXC executor" default:"wxc-exec.exe" fileIgnoreEmpty:"true" fileStorage:"value"`
	Version           string   `config:"version" env:"CRABBOX_MXC_VERSION" flag:"mxc-version" sources:"user,repo,env,flag" help:"MXC configuration schema version" default:"0.6.0-alpha" fileIgnoreEmpty:"true" fileStorage:"value"`
	Containment       string   `config:"containment" env:"CRABBOX_MXC_CONTAINMENT" flag:"mxc-containment" sources:"user,repo,env,flag" help:"MXC containment backend" default:"processcontainer" fileIgnoreEmpty:"true" fileStorage:"value"`
	Network           string   `config:"network" env:"CRABBOX_MXC_NETWORK" flag:"mxc-network" sources:"user,repo,env,flag" help:"MXC network default: block or allow" default:"block" fileIgnoreEmpty:"true" fileStorage:"value"`
	ReadOnlyPaths     []string `config:"readOnlyPaths" env:"CRABBOX_MXC_READONLY_PATHS" flag:"mxc-readonly-paths" sources:"user,repo,env,flag" help:"comma-separated additional read-only paths" fileList:"raw" fileStorage:"value" flagList:"scalar-empty-nil"`
	ReadWritePaths    []string `config:"readWritePaths" env:"CRABBOX_MXC_READWRITE_PATHS" flag:"mxc-readwrite-paths" sources:"user,repo,env,flag" help:"comma-separated additional read-write paths" fileList:"raw" fileStorage:"value" flagList:"scalar-empty-nil"`
	AllowedHosts      []string `config:"allowedHosts" env:"CRABBOX_MXC_ALLOWED_HOSTS" flag:"mxc-allowed-hosts" sources:"user,repo,env,flag" help:"comma-separated allowed outbound hosts" fileList:"raw" fileStorage:"value" flagList:"scalar-empty-nil"`
	BlockedHosts      []string `config:"blockedHosts" env:"CRABBOX_MXC_BLOCKED_HOSTS" flag:"mxc-blocked-hosts" sources:"user,repo,env,flag" help:"comma-separated blocked outbound hosts" fileList:"raw" fileStorage:"value" flagList:"scalar-empty-nil"`
	AllowDACLMutation bool     `config:"allowDaclMutation" env:"CRABBOX_MXC_ALLOW_DACL_MUTATION" flag:"mxc-allow-dacl-mutation" sources:"user,repo,env,flag" help:"allow MXC to mutate host ACLs for its AppContainer fallback"`
	AllowWindowsUI    bool     `config:"allowWindowsUI" env:"CRABBOX_MXC_ALLOW_WINDOWS_UI" flag:"mxc-allow-windows-ui" sources:"user,repo,env,flag" help:"allow Win32k/UI system calls required by programs such as Windows PowerShell"`
	Experimental      bool     `config:"experimental" env:"CRABBOX_MXC_EXPERIMENTAL" flag:"mxc-experimental" sources:"user,repo,env,flag" help:"enable experimental MXC containment backends"`
}
