package cli

import "strings"

//go:generate go run ../../scripts/configgen -source config_nvidia_brev.go -output config_nvidia_brev_generated.go -type NvidiaBrevConfig -provider nvidia-brev

// NvidiaBrevConfig is intentionally non-secret. Authentication stays in the
// NVIDIA Brev CLI's own credential store and is never accepted as Crabbox
// config or argv.
type NvidiaBrevConfig struct {
	CLI        string `config:"cli" env:"CRABBOX_NVIDIA_BREV_CLI" flag:"nvidia-brev-cli" sources:"user,env,flag" help:"NVIDIA Brev CLI path" default:"brev" fileIgnoreEmpty:"true" fileStorage:"value"`
	Org        string `config:"org" env:"CRABBOX_NVIDIA_BREV_ORG" flag:"nvidia-brev-org" sources:"user,repo,env,flag" help:"NVIDIA Brev organization selector" fileIgnoreEmpty:"true" fileStorage:"value"`
	Type       string `config:"type" env:"CRABBOX_NVIDIA_BREV_TYPE" flag:"nvidia-brev-type" sources:"user,repo,env,flag" help:"NVIDIA Brev instance type selector" fileIgnoreEmpty:"true" fileStorage:"value"`
	GPUName    string `config:"gpuName" env:"CRABBOX_NVIDIA_BREV_GPU_NAME" flag:"nvidia-brev-gpu-name" sources:"user,repo,env,flag" help:"NVIDIA Brev GPU name selector" default:"A100" fileIgnoreEmpty:"true" fileStorage:"value"`
	Provider   string `config:"provider" env:"CRABBOX_NVIDIA_BREV_PROVIDER" flag:"nvidia-brev-provider" sources:"user,repo,env,flag" help:"NVIDIA Brev cloud provider selector" fileIgnoreEmpty:"true" fileStorage:"value"`
	Mode       string `config:"mode" env:"CRABBOX_NVIDIA_BREV_MODE" flag:"nvidia-brev-mode" sources:"user,repo,env,flag" help:"NVIDIA Brev mode: vm" default:"vm" fileIgnoreEmpty:"true" fileStorage:"value"`
	Launchable string `config:"launchable" env:"CRABBOX_NVIDIA_BREV_LAUNCHABLE" flag:"nvidia-brev-launchable" sources:"user,repo,env,flag" help:"NVIDIA Brev launchable selector" fileIgnoreEmpty:"true" fileStorage:"value"`
	// applyNvidiaBrevFileConfig owns the additional startup-script file admission.
	StartupScript string `config:"startupScript" env:"CRABBOX_NVIDIA_BREV_STARTUP_SCRIPT" flag:"nvidia-brev-startup-script" sources:"user,repo,env,flag" help:"NVIDIA Brev startup script inline command or @file path" fileIgnoreEmpty:"true" fileStorage:"value"`
	ReleaseAction string `config:"releaseAction" env:"CRABBOX_NVIDIA_BREV_RELEASE_ACTION" flag:"nvidia-brev-release-action" sources:"user,repo,env,flag" help:"NVIDIA Brev release action: delete or stop" default:"delete" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Target        string `config:"target" env:"CRABBOX_NVIDIA_BREV_TARGET" flag:"nvidia-brev-target" sources:"user,repo,env,flag" help:"NVIDIA Brev SSH target: container or host" default:"container" fileIgnoreEmpty:"true" fileStorage:"value"`
	User          string `config:"user" env:"CRABBOX_NVIDIA_BREV_USER" flag:"nvidia-brev-user" sources:"user,repo,env,flag" help:"SSH user for NVIDIA Brev workspaces" fileIgnoreEmpty:"true" fileStorage:"value"`
	WorkRoot      string `config:"workRoot" env:"CRABBOX_NVIDIA_BREV_WORK_ROOT" flag:"nvidia-brev-work-root" sources:"user,repo,env,flag" help:"remote Crabbox work root on NVIDIA Brev workspaces" default:"/tmp/crabbox" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
}

// WithRuntimeDefaults fills raw empty settings; WorkRoot retains its explicit-input resolver.
func (cfg NvidiaBrevConfig) WithRuntimeDefaults() NvidiaBrevConfig {
	defaults := defaultNvidiaBrevConfig()
	if cfg.CLI == "" {
		cfg.CLI = defaults.CLI
	}
	if cfg.GPUName == "" {
		cfg.GPUName = defaults.GPUName
	}
	if cfg.Mode == "" {
		cfg.Mode = defaults.Mode
	}
	if cfg.ReleaseAction == "" {
		cfg.ReleaseAction = defaults.ReleaseAction
	}
	if cfg.Target == "" {
		cfg.Target = defaults.Target
	}
	return cfg
}

// Keep startup-script admission local to this snapshot, never the persisted file DTO.
func applyNvidiaBrevFileConfig(cfg *Config, file *fileNvidiaBrevConfig, trusted bool, source configInputSource) error {
	if file == nil {
		return nil
	}
	snapshot := *file
	if snapshot.StartupScript != "" &&
		!(trusted || !strings.HasPrefix(strings.TrimSpace(snapshot.StartupScript), "@")) {
		snapshot.StartupScript = ""
	}
	applied, err := cfg.NvidiaBrev.applyFile(&snapshot, trusted)
	recordConfigInput(cfg, "nvidia-brev", source, applied.InputAccepted)
	if applied.ReleaseAction {
		MarkDeleteOnReleaseExplicit(cfg, "nvidia-brev")
	}
	if applied.WorkRoot {
		MarkNvidiaBrevWorkRootExplicit(cfg)
	}
	return err
}
