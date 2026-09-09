package cli

//go:generate go run ../../scripts/configgen -source config_linode.go -output config_linode_generated.go -type LinodeConfig -provider linode

const (
	LinodeConfiguredRegionDefault = "us-ord"
	LinodeConfiguredTypeDefault   = "g6-standard-1"
	LinodeImageFallback           = "linode/ubuntu24.04"
)

// LinodeConfig contains non-secret settings without provider flags.
// Portable OS resolution and late native validation remain separate policy.
type LinodeConfig struct {
	Region     string   `config:"region" env:"CRABBOX_LINODE_REGION" sources:"user,repo,env" fileIgnoreEmpty:"true"`
	Image      string   `config:"image" env:"CRABBOX_LINODE_IMAGE" sources:"user,repo,env" fileIgnoreEmpty:"true" reportApplied:"true"`
	Type       string   `config:"type" env:"CRABBOX_LINODE_TYPE" sources:"user,repo,env" fileIgnoreEmpty:"true" reportApplied:"true"`
	FirewallID string   `config:"firewall" env:"CRABBOX_LINODE_FIREWALL" sources:"user,repo,env" fileIgnoreEmpty:"true"`
	SSHCIDRs   []string `config:"sshCIDRs" env:"CRABBOX_LINODE_SSH_CIDRS" sources:"user,repo,env" fileList:"nonempty-raw"`
}

// initialLinodeConfig preserves core's already-resolved image, including empty.
func initialLinodeConfig(osDerivedImage string) LinodeConfig {
	cfg := defaultLinodeConfig()
	cfg.Region = LinodeConfiguredRegionDefault
	cfg.Image = osDerivedImage
	cfg.Type = LinodeConfiguredTypeDefault
	return cfg
}
