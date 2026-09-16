package cli

//go:generate go run ../../scripts/configgen -source config_nebius.go -output config_nebius_generated.go -type NebiusConfig -provider nebius

// NebiusConfig is intentionally non-secret. Authentication stays in the
// Nebius CLI profile store and is never accepted as Crabbox config or argv.
type NebiusConfig struct {
	CLI              string   `config:"cli" env:"CRABBOX_NEBIUS_CLI" flag:"nebius-cli" sources:"user,env,flag" help:"Nebius CLI path" default:"nebius" fileIgnoreEmpty:"true" fileStorage:"value"`
	Profile          string   `config:"profile" env:"CRABBOX_NEBIUS_PROFILE" flag:"nebius-profile" sources:"user,env,flag" help:"Nebius CLI profile name" fileIgnoreEmpty:"true" fileStorage:"value"`
	ParentID         string   `config:"parentId" env:"CRABBOX_NEBIUS_PARENT_ID" flag:"nebius-parent-id" sources:"user,repo,env,flag" help:"Nebius parent/project ID" fileIgnoreEmpty:"true" fileStorage:"value"`
	SubnetID         string   `config:"subnetId" env:"CRABBOX_NEBIUS_SUBNET_ID" flag:"nebius-subnet-id" sources:"user,repo,env,flag" help:"Nebius subnet ID" fileIgnoreEmpty:"true" fileStorage:"value"`
	Platform         string   `config:"platform" env:"CRABBOX_NEBIUS_PLATFORM" flag:"nebius-platform" sources:"user,repo,env,flag" help:"Nebius compute platform" default:"cpu-d3" fileIgnoreEmpty:"true" fileStorage:"value"`
	Preset           string   `config:"preset" env:"CRABBOX_NEBIUS_PRESET" flag:"nebius-preset" sources:"user,repo,env,flag" help:"Nebius compute preset" default:"4vcpu-16gb" fileIgnoreEmpty:"true" fileStorage:"value"`
	ImageFamily      string   `config:"imageFamily" env:"CRABBOX_NEBIUS_IMAGE_FAMILY" flag:"nebius-image-family" sources:"user,repo,env,flag" help:"Nebius boot image family" default:"ubuntu24.04-driverless" fileIgnoreEmpty:"true" fileStorage:"value"`
	DiskType         string   `config:"diskType" env:"CRABBOX_NEBIUS_DISK_TYPE" flag:"nebius-disk-type" sources:"user,repo,env,flag" help:"Nebius boot disk type" default:"network_ssd" fileIgnoreEmpty:"true" fileStorage:"value"`
	DiskSizeGiB      int      `config:"diskSizeGiB" env:"CRABBOX_NEBIUS_DISK_SIZE_GIB" flag:"nebius-disk-size-gib" sources:"user,repo,env,flag" help:"Nebius boot disk size in GiB" default:"50" nonnegative:"true" fileInt:"positive" envInt:"fallback" fileStorage:"value"`
	User             string   `config:"user" env:"CRABBOX_NEBIUS_USER" flag:"nebius-user" sources:"user,repo,env,flag" help:"SSH user for Nebius VMs" default:"crabbox" fileIgnoreEmpty:"true" fileStorage:"value"`
	PublicIP         string   `config:"publicIP" env:"CRABBOX_NEBIUS_PUBLIC_IP" flag:"nebius-public-ip" sources:"user,repo,env,flag" help:"Nebius public IP mode: dynamic or none" default:"dynamic" fileIgnoreEmpty:"true" fileStorage:"value"`
	SecurityGroupIDs []string `config:"securityGroupIds" env:"CRABBOX_NEBIUS_SECURITY_GROUP_IDS" flag:"nebius-security-group-ids" sources:"user,repo,env,flag" help:"comma-separated Nebius security group IDs" fileList:"nonempty-raw" flagList:"empty-scalar" fileStorage:"value"`
	ServiceAccountID string   `config:"serviceAccountId" env:"CRABBOX_NEBIUS_SERVICE_ACCOUNT_ID" flag:"nebius-service-account-id" sources:"user,env,flag" help:"Nebius service account ID for VMs" fileIgnoreEmpty:"true" fileStorage:"value"`
	RecoveryPolicy   string   `config:"recoveryPolicy" env:"CRABBOX_NEBIUS_RECOVERY_POLICY" flag:"nebius-recovery-policy" sources:"user,repo,env,flag" help:"Nebius create recovery policy: fail" default:"fail" fileIgnoreEmpty:"true" fileStorage:"value"`
}

// WithRuntimeDefaults fills only the existing raw-empty strings and zero disk
// size. Other settings and slice identity remain unchanged.
func (cfg NebiusConfig) WithRuntimeDefaults() NebiusConfig {
	defaults := defaultNebiusConfig()
	if cfg.CLI == "" {
		cfg.CLI = defaults.CLI
	}
	if cfg.Platform == "" {
		cfg.Platform = defaults.Platform
	}
	if cfg.Preset == "" {
		cfg.Preset = defaults.Preset
	}
	if cfg.ImageFamily == "" {
		cfg.ImageFamily = defaults.ImageFamily
	}
	if cfg.DiskType == "" {
		cfg.DiskType = defaults.DiskType
	}
	if cfg.DiskSizeGiB == 0 {
		cfg.DiskSizeGiB = defaults.DiskSizeGiB
	}
	if cfg.User == "" {
		cfg.User = defaults.User
	}
	if cfg.PublicIP == "" {
		cfg.PublicIP = defaults.PublicIP
	}
	if cfg.RecoveryPolicy == "" {
		cfg.RecoveryPolicy = defaults.RecoveryPolicy
	}
	return cfg
}
