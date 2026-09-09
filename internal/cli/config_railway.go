package cli

//go:generate go run ../../scripts/configgen -source config_railway.go -output config_railway_generated.go -type RailwayConfig -provider railway

// RailwayConfig describes mechanical bindings for the service-control provider.
// Credential provenance and destination validation remain owned by core policy.
type RailwayConfig struct {
	APIToken      string `env:"CRABBOX_RAILWAY_API_TOKEN" envAlias:"RAILWAY_API_TOKEN" sources:"env" reportApplied:"true"`
	APIURL        string `config:"apiUrl" env:"CRABBOX_RAILWAY_API_URL" envAlias:"RAILWAY_API_URL" flag:"railway-url" sources:"user,repo,env,flag" help:"Railway GraphQL API URL" default:"https://backboard.railway.com/graphql/v2" fileIgnoreEmpty:"true" reportApplied:"true"`
	ProjectID     string `config:"projectId" env:"CRABBOX_RAILWAY_PROJECT_ID" envAlias:"RAILWAY_PROJECT_ID" flag:"railway-project" sources:"user,repo,env,flag" help:"Railway project ID containing the target service" fileIgnoreEmpty:"true"`
	EnvironmentID string `config:"environmentId" env:"CRABBOX_RAILWAY_ENVIRONMENT_ID" envAlias:"RAILWAY_ENVIRONMENT_ID" flag:"railway-environment" sources:"user,repo,env,flag" help:"Railway environment ID to deploy into" fileIgnoreEmpty:"true"`
}
