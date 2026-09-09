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
	Region        string   `config:"region" env:"CRABBOX_VULTR_REGION" sources:"user,repo,env" fileIgnoreEmpty:"true"`
	OS            string   `config:"os" env:"CRABBOX_VULTR_OS" sources:"user,repo,env" fileIgnoreEmpty:"true"`
	Image         string   `config:"image" env:"CRABBOX_VULTR_IMAGE" sources:"user,repo,env" fileIgnoreEmpty:"true"`
	Snapshot      string   `config:"snapshot" env:"CRABBOX_VULTR_SNAPSHOT" sources:"user,repo,env" fileIgnoreEmpty:"true"`
	FirewallGroup string   `config:"firewallGroup" env:"CRABBOX_VULTR_FIREWALL_GROUP" sources:"user,repo,env" fileIgnoreEmpty:"true"`
	VPCIDs        []string `config:"vpcIds" env:"CRABBOX_VULTR_VPC_IDS" sources:"user,repo,env" fileList:"nonempty-raw"`
	SSHCIDRs      []string `config:"sshCIDRs" env:"CRABBOX_VULTR_SSH_CIDRS" sources:"user,repo,env" fileList:"nonempty-raw"`
	UserScheme    string   `config:"userScheme" env:"CRABBOX_VULTR_USER_SCHEME" sources:"user,repo,env" fileIgnoreEmpty:"true"`
}
