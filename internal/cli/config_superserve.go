package cli

import "strings"

//go:generate go run ../../scripts/configgen -source config_superserve.go -output config_superserve_generated.go -type SuperserveConfig -provider superserve

// SuperserveConfig owns mechanical bindings. The runtime API key remains absent
// from configuration and argv; provider validation stays in its existing phase.
type SuperserveConfig struct {
	BaseURL         string   `config:"baseUrl" env:"CRABBOX_SUPERSERVE_BASE_URL" envAlias:"SUPERSERVE_BASE_URL" flag:"superserve-base-url" sources:"user,env,flag" help:"Trusted Superserve API base URL" default:"https://api.superserve.ai" fileIgnoreEmpty:"true" fileStorage:"value"`
	Template        string   `config:"template" env:"CRABBOX_SUPERSERVE_TEMPLATE" flag:"superserve-template" sources:"user,repo,env,flag" help:"Superserve sandbox template" default:"superserve/base"`
	Snapshot        string   `config:"snapshot" env:"CRABBOX_SUPERSERVE_SNAPSHOT" flag:"superserve-snapshot" sources:"user,repo,env,flag" help:"Superserve snapshot ID or name"`
	Workdir         string   `config:"workdir" env:"CRABBOX_SUPERSERVE_WORKDIR" flag:"superserve-workdir" sources:"user,repo,env,flag" help:"Absolute working directory inside the sandbox" default:"/workspace/crabbox"`
	TimeoutSecs     int      `config:"timeoutSecs" env:"CRABBOX_SUPERSERVE_TIMEOUT_SECS" flag:"superserve-timeout-secs" sources:"user,repo,env,flag" help:"Superserve sandbox lifetime cap in seconds (0 = Crabbox TTL)" nonnegative:"true"`
	ExecTimeoutSecs int      `config:"execTimeoutSecs" env:"CRABBOX_SUPERSERVE_EXEC_TIMEOUT_SECS" flag:"superserve-exec-timeout-secs" sources:"user,repo,env,flag" help:"Superserve command timeout in seconds (0 = service default)" nonnegative:"true" default:"600"`
	NetworkAllowOut []string `config:"networkAllowOut" env:"CRABBOX_SUPERSERVE_NETWORK_ALLOW_OUT" flag:"superserve-network-allow-out" sources:"user,repo,env,flag" help:"comma-separated outbound network allow list" fileList:"present-normalized" fileStorage:"value"`
	NetworkDenyOut  []string `config:"networkDenyOut" env:"CRABBOX_SUPERSERVE_NETWORK_DENY_OUT" flag:"superserve-network-deny-out" sources:"user,repo,env,flag" help:"comma-separated outbound network deny list" fileList:"present-normalized" fileStorage:"value"`
	ForgetMissing   bool     `config:"forgetMissing" env:"CRABBOX_SUPERSERVE_FORGET_MISSING" flag:"superserve-forget-missing" sources:"user,repo,env,flag" help:"remove the local claim when stop gets 404 (explicit stale-claim cleanup)"`
}

func applySuperserveFileConfig(cfg *Config, file *fileSuperserveConfig, trusted bool, source configInputSource) error {
	if file == nil {
		return nil
	}
	// Ignore a trimmed-blank URL only for application; preserve the persisted DTO.
	snapshot := *file
	if strings.TrimSpace(snapshot.BaseURL) == "" {
		snapshot.BaseURL = ""
	}
	applied, err := cfg.Superserve.applyFile(&snapshot, trusted)
	recordConfigInput(cfg, "superserve", source, applied.InputAccepted)
	return err
}

func applySuperserveEnvironmentConfig(cfg *Config) error {
	applied, err := cfg.Superserve.applyEnv()
	recordConfigInput(cfg, "superserve", configInputEnvironment, applied.InputAccepted)
	return err
}
