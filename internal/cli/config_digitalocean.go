package cli

//go:generate go run ../../scripts/configgen -source config_digitalocean.go -output config_digitalocean_generated.go -type DigitalOceanConfig -provider digitalocean

// Runtime fallbacks do not initialize the raw provider configuration.
const (
	DigitalOceanRegionFallback = "nyc3"
	DigitalOceanImageFallback  = "ubuntu-24-04-x64"
)

// DigitalOceanConfig contains non-secret settings without provider flags.
// Token loading and portable OS selection remain separate policy.
type DigitalOceanConfig struct {
	Region   string   `config:"region" env:"CRABBOX_DIGITALOCEAN_REGION" sources:"user,repo,env" fileIgnoreEmpty:"true"`
	Image    string   `config:"image" env:"CRABBOX_DIGITALOCEAN_IMAGE" sources:"user,repo,env" fileIgnoreEmpty:"true" reportApplied:"true"`
	VPCUUID  string   `config:"vpc" env:"CRABBOX_DIGITALOCEAN_VPC" sources:"user,repo,env" fileIgnoreEmpty:"true"`
	SSHCIDRs []string `config:"sshCIDRs" env:"CRABBOX_DIGITALOCEAN_SSH_CIDRS" sources:"user,repo,env" fileList:"nonempty-raw"`
}
