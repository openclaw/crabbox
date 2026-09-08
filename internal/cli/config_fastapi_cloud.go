package cli

//go:generate go run ../../scripts/configgen -source config_fastapi_cloud.go -output config_fastapi_cloud_generated.go -type FastAPICloudConfig -provider fastapi-cloud

// FastAPICloudConfig describes mechanical bindings for the service-control provider.
// Credential provenance and destination validation remain owned by core policy.
type FastAPICloudConfig struct {
	Token  string `env:"CRABBOX_FASTAPI_CLOUD_TOKEN" envAlias:"FASTAPI_CLOUD_TOKEN" sources:"env" reportApplied:"true"`
	APIURL string `config:"apiUrl" env:"CRABBOX_FASTAPI_CLOUD_API_URL" envAlias:"FASTAPI_CLOUD_API_URL" flag:"fastapi-cloud-url" sources:"user,repo,env,flag" help:"FastAPI Cloud API URL" default:"https://api.fastapicloud.com/api/v1" fileIgnoreEmpty:"true" reportApplied:"true"`
	AppID  string `config:"appId" env:"CRABBOX_FASTAPI_CLOUD_APP_ID" envAlias:"FASTAPI_CLOUD_APP_ID" flag:"fastapi-cloud-app-id" sources:"user,repo,env,flag" help:"FastAPI Cloud app ID" fileIgnoreEmpty:"true"`
	TeamID string `config:"teamId" env:"CRABBOX_FASTAPI_CLOUD_TEAM_ID" envAlias:"FASTAPI_CLOUD_TEAM_ID" flag:"fastapi-cloud-team-id" sources:"user,repo,env,flag" help:"FastAPI Cloud team ID for listing apps" fileIgnoreEmpty:"true"`
}
