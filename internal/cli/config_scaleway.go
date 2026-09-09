package cli

//go:generate go run ../../scripts/configgen -source config_scaleway.go -output config_scaleway_generated.go -type ScalewayConfig -provider scaleway

// ScalewayConfig contains non-secret Scaleway Instances settings. SDK profile
// selection and native validation remain provider policy.
type ScalewayConfig struct {
	Region         string   `config:"region" env:"CRABBOX_SCALEWAY_REGION" flag:"scaleway-region" sources:"user,repo,env,flag" help:"Scaleway region" default:"fr-par" fileIgnoreEmpty:"true" reportApplied:"true"`
	Zone           string   `config:"zone" env:"CRABBOX_SCALEWAY_ZONE" flag:"scaleway-zone" sources:"user,repo,env,flag" help:"Scaleway zone" default:"fr-par-1" fileIgnoreEmpty:"true" reportApplied:"true"`
	Image          string   `config:"image" env:"CRABBOX_SCALEWAY_IMAGE" flag:"scaleway-image" sources:"user,repo,env,flag" help:"Scaleway image label or ID" default:"ubuntu_noble" fileIgnoreEmpty:"true" reportApplied:"true"`
	Type           string   `config:"type" env:"CRABBOX_SCALEWAY_TYPE" flag:"scaleway-type" sources:"user,repo,env,flag" help:"Scaleway Instances commercial type" default:"DEV1-S" fileIgnoreEmpty:"true" reportApplied:"true"`
	ProjectID      string   `config:"projectId" env:"CRABBOX_SCALEWAY_PROJECT_ID" flag:"scaleway-project-id" sources:"user,repo,env,flag" help:"Scaleway project ID" fileIgnoreEmpty:"true"`
	OrganizationID string   `config:"organizationId" env:"CRABBOX_SCALEWAY_ORGANIZATION_ID" flag:"scaleway-organization-id" sources:"user,repo,env,flag" help:"Scaleway organization ID" fileIgnoreEmpty:"true"`
	SecurityGroup  string   `config:"securityGroup" env:"CRABBOX_SCALEWAY_SECURITY_GROUP" flag:"scaleway-security-group" sources:"user,repo,env,flag" help:"Scaleway security group ID" fileIgnoreEmpty:"true"`
	SSHCIDRs       []string `config:"sshCIDRs" env:"CRABBOX_SCALEWAY_SSH_CIDRS" flag:"scaleway-ssh-cidrs" sources:"user,repo,env,flag" help:"comma-separated Scaleway SSH source CIDRs" fileList:"nonempty-raw" flagList:"empty-scalar"`
}

// MarkScalewayConfigApplied records accepted source events without inferring
// explicitness from values or changing SDK profile precedence.
func MarkScalewayConfigApplied(cfg *Config, applied ScalewayConfigApplied) {
	if applied.Region {
		SetScalewayRegionExplicit(cfg)
	}
	if applied.Zone {
		SetScalewayZoneExplicit(cfg)
	}
	if applied.Image {
		SetScalewayImageExplicit(cfg)
	}
	if applied.Type {
		SetScalewayTypeExplicit(cfg)
	}
}
