package cli

//go:generate go run ../../scripts/configgen -source config_phala.go -output config_phala_generated.go -type PhalaConfig -provider phala

// PhalaConfig holds ordinary settings, not the provider's native credentials.
//
//configgen:flag-application manual
type PhalaConfig struct {
	CLIPath      string `config:"cli" env:"CRABBOX_PHALA_CLI" flag:"phala-cli" help:"Phala CLI path" sources:"user,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"phala" reportApplied:"true"`
	InstanceType string `config:"instanceType" env:"CRABBOX_PHALA_INSTANCE_TYPE" flag:"phala-instance-type" help:"Phala confidential TDX instance type, for example tdx.small" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"tdx.small" reportApplied:"true"`
	// The dev OS root is read-only; this dedicated writable tmpfs holds the workspace.
	WorkRoot string `config:"workRoot" env:"CRABBOX_PHALA_WORK_ROOT" flag:"phala-work-root" help:"remote Crabbox work root" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"/var/volatile/crabbox"`
	NodeID   string `config:"nodeId" env:"CRABBOX_PHALA_NODE_ID" flag:"phala-node-id" help:"Phala node id to pin deployments to" sources:"user,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	Compose  string `config:"compose" env:"CRABBOX_PHALA_COMPOSE" flag:"phala-compose" help:"optional Docker Compose file deployed alongside the dev OS" sources:"user,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	// Nil retains the provider's existing effective default. File admission is wrapper-owned.
	Attest *bool `config:"attest" env:"CRABBOX_PHALA_ATTEST" flag:"phala-attest" help:"verify the leased CVM's Intel TDX remote attestation before trusting it (default true)" sources:"user,repo,env,flag"`
}

func (cfg *PhalaConfig) ExpandAppliedLocalPaths(applied PhalaConfigApplied) {
	if applied.CLIPath {
		cfg.CLIPath = expandUserPath(cfg.CLIPath)
	}
	if applied.Compose {
		cfg.Compose = expandUserPath(cfg.Compose)
	}
}

// Keep the existing conditional admission on a copy, never the persisted file DTO.
func applyPhalaFileConfig(cfg *Config, file *filePhalaConfig, trusted bool, source configInputSource) error {
	if file == nil {
		return nil
	}
	snapshot := *file
	if snapshot.Attest != nil && !(trusted || *snapshot.Attest) {
		snapshot.Attest = nil
	}
	applied, err := cfg.Phala.applyFile(&snapshot, trusted)
	recordConfigInput(cfg, "phala", source, applied.InputAccepted)
	cfg.Phala.ExpandAppliedLocalPaths(applied)
	if applied.InstanceType {
		MarkPhalaInstanceTypeExplicit(cfg)
	}
	return err
}
