package cli

//go:generate go run ../../scripts/configgen -source config_windows_sandbox.go -output config_windows_sandbox_generated.go -type WindowsSandboxConfig -provider windows-sandbox

// WindowsSandboxConfig owns input bindings; ordered flag validation stays in the provider.
//
//configgen:flag-application manual
type WindowsSandboxConfig struct {
	Workdir            string `config:"workdir" env:"CRABBOX_WINDOWS_SANDBOX_WORKDIR" flag:"windows-sandbox-workdir" sources:"user,repo,env,flag" help:"absolute working directory inside Windows Sandbox" default:"C:\\crabbox-work" fileIgnoreEmpty:"true" fileStorage:"value"`
	TempRoot           string `config:"tempRoot" env:"CRABBOX_WINDOWS_SANDBOX_TEMP_ROOT" flag:"windows-sandbox-temp-root" sources:"user,env,flag" help:"host directory for temporary Windows Sandbox workspaces" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Networking         string `config:"networking" env:"CRABBOX_WINDOWS_SANDBOX_NETWORKING" flag:"windows-sandbox-networking" sources:"user,env,flag" help:"Windows Sandbox networking: enable, disable, or default" default:"Enable" fileIgnoreEmpty:"true" fileStorage:"value"`
	VGPU               string `config:"vgpu" env:"CRABBOX_WINDOWS_SANDBOX_VGPU" flag:"windows-sandbox-vgpu" sources:"user,env,flag" help:"Windows Sandbox vGPU: enable, disable, or default" default:"Disable" fileIgnoreEmpty:"true" fileStorage:"value"`
	Clipboard          string `config:"clipboard" env:"CRABBOX_WINDOWS_SANDBOX_CLIPBOARD" flag:"windows-sandbox-clipboard" sources:"user,env,flag" help:"Windows Sandbox clipboard redirection: enable, disable, or default" default:"Disable" fileIgnoreEmpty:"true" fileStorage:"value"`
	ProtectedClient    string `config:"protectedClient" env:"CRABBOX_WINDOWS_SANDBOX_PROTECTED_CLIENT" flag:"windows-sandbox-protected-client" sources:"user,env,flag" help:"Windows Sandbox protected client: enable, disable, or default" default:"Default" fileIgnoreEmpty:"true" fileStorage:"value"`
	AudioInput         string `config:"audioInput" env:"CRABBOX_WINDOWS_SANDBOX_AUDIO_INPUT" flag:"windows-sandbox-audio-input" sources:"user,env,flag" help:"Windows Sandbox audio input: enable, disable, or default" default:"Disable" fileIgnoreEmpty:"true" fileStorage:"value"`
	VideoInput         string `config:"videoInput" env:"CRABBOX_WINDOWS_SANDBOX_VIDEO_INPUT" flag:"windows-sandbox-video-input" sources:"user,env,flag" help:"Windows Sandbox video input: enable, disable, or default" default:"Disable" fileIgnoreEmpty:"true" fileStorage:"value"`
	PrinterRedirection string `config:"printerRedirection" env:"CRABBOX_WINDOWS_SANDBOX_PRINTER_REDIRECTION" flag:"windows-sandbox-printer-redirection" sources:"user,env,flag" help:"Windows Sandbox printer redirection: enable, disable, or default" default:"Disable" fileIgnoreEmpty:"true" fileStorage:"value"`
	MemoryMB           int    `config:"memoryMB" env:"CRABBOX_WINDOWS_SANDBOX_MEMORY_MB" flag:"windows-sandbox-memory-mb" sources:"user,env,flag" help:"Windows Sandbox memory in MB; 0 leaves the platform default" nonnegative:"true" fileInt:"positive" fileStorage:"value" envInt:"fallback"`
}

func applyWindowsSandboxFileConfig(cfg *Config, file *fileWindowsSandboxConfig, trusted bool, source configInputSource) error {
	applied, err := cfg.WindowsSandbox.applyFile(file, trusted)
	recordConfigInput(cfg, "windows-sandbox", source, applied.InputAccepted)
	if applied.TempRoot {
		cfg.WindowsSandbox.TempRoot = expandUserPath(cfg.WindowsSandbox.TempRoot)
	}
	return err
}

func applyWindowsSandboxEnvironmentConfig(cfg *Config) error {
	applied, err := cfg.WindowsSandbox.applyEnv()
	recordConfigInput(cfg, "windows-sandbox", configInputEnvironment, applied.InputAccepted)
	// Environment processing also expands an inherited path when no input was accepted.
	cfg.WindowsSandbox.TempRoot = expandUserPath(cfg.WindowsSandbox.TempRoot)
	return err
}
