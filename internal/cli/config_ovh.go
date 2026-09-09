package cli

//go:generate go run ../../scripts/configgen -source config_ovh.go -output config_ovh_generated.go -type OVHConfig -provider ovh

// OVHConfig contains non-secret OVHcloud Public Cloud settings. OVH
// application credentials are intentionally read from environment variables by
// the provider client and are not persisted in Crabbox config.
type OVHConfig struct {
	Endpoint  string `config:"endpoint" env:"OVH_ENDPOINT" flag:"ovh-endpoint" sources:"user,env,flag" help:"OVHcloud API endpoint" default:"https://api.us.ovhcloud.com/1.0" fileIgnoreEmpty:"true" fileStorage:"value"`
	ProjectID string `config:"projectId" env:"CRABBOX_OVH_PROJECT_ID" flag:"ovh-project-id" sources:"user,repo,env,flag" help:"OVHcloud Public Cloud project ID" fileIgnoreEmpty:"true" fileStorage:"value"`
	Region    string `config:"region" env:"CRABBOX_OVH_REGION" flag:"ovh-region" sources:"user,repo,env,flag" help:"OVHcloud Public Cloud region" fileIgnoreEmpty:"true" fileStorage:"value"`
	Image     string `config:"image" env:"CRABBOX_OVH_IMAGE" flag:"ovh-image" sources:"user,repo,env,flag" help:"OVHcloud Public Cloud image name or ID" default:"Ubuntu 24.04" fileIgnoreEmpty:"true" reportApplied:"true" fileStorage:"value"`
	Flavor    string `config:"flavor" env:"CRABBOX_OVH_FLAVOR" flag:"ovh-flavor" sources:"user,repo,env,flag" help:"OVHcloud Public Cloud flavor name or ID" default:"b3-8" fileIgnoreEmpty:"true" fileStorage:"value"`
}
