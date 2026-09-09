package cli

//go:generate go run ../../scripts/configgen -source config_vultr.go -output config_vultr_generated.go -type VultrConfig -provider vultr

// Runtime fallbacks do not initialize the raw provider configuration.
const (
	VultrRegionFallback     = "ewr"
	VultrUserSchemeFallback = "root"
)

// VultrConfig contains non-secret settings without provider flags.
// Boot-source selection and native validation remain provider policy.
type VultrConfig struct {
	Region        string   `config:"region" env:"CRABBOX_VULTR_REGION" sources:"user,repo,env" fileIgnoreEmpty:"true" fileStorage:"value"`
	OS            string   `config:"os" env:"CRABBOX_VULTR_OS" sources:"user,repo,env" fileIgnoreEmpty:"true" fileStorage:"value"`
	Image         string   `config:"image" env:"CRABBOX_VULTR_IMAGE" sources:"user,repo,env" fileIgnoreEmpty:"true" fileStorage:"value"`
	Snapshot      string   `config:"snapshot" env:"CRABBOX_VULTR_SNAPSHOT" sources:"user,repo,env" fileIgnoreEmpty:"true" fileStorage:"value"`
	FirewallGroup string   `config:"firewallGroup" env:"CRABBOX_VULTR_FIREWALL_GROUP" sources:"user,repo,env" fileIgnoreEmpty:"true" fileStorage:"value"`
	VPCIDs        []string `config:"vpcIds" env:"CRABBOX_VULTR_VPC_IDS" sources:"user,repo,env" fileList:"nonempty-raw" fileStorage:"value"`
	SSHCIDRs      []string `config:"sshCIDRs" env:"CRABBOX_VULTR_SSH_CIDRS" sources:"user,repo,env" fileList:"nonempty-raw" fileStorage:"value"`
	UserScheme    string   `config:"userScheme" env:"CRABBOX_VULTR_USER_SCHEME" sources:"user,repo,env" fileIgnoreEmpty:"true" fileStorage:"value"`
}

// WithRuntimeDefaults returns a shallow copy with only raw-empty Region and
// UserScheme filled. It does not normalize values or select generic SSH policy.
func (cfg VultrConfig) WithRuntimeDefaults() VultrConfig {
	if cfg.Region == "" {
		cfg.Region = VultrRegionFallback
	}
	if cfg.UserScheme == "" {
		cfg.UserScheme = VultrUserSchemeFallback
	}
	return cfg
}
