package cli

//go:generate go run ../../scripts/configgen -source config_e2b.go -output config_e2b_generated.go -type E2BConfig -provider e2b

// E2BConfig describes mechanical bindings for the delegated provider.
// Credential provenance and destination validation remain owned by core policy.
type E2BConfig struct {
	APIKey   string `env:"CRABBOX_E2B_API_KEY" envAlias:"E2B_API_KEY" sources:"env" reportApplied:"true"`
	APIURL   string `config:"apiUrl" env:"CRABBOX_E2B_API_URL" envAlias:"E2B_API_URL" flag:"e2b-api-url" sources:"user,repo,env,flag" help:"E2B API URL" default:"https://api.e2b.app" fileIgnoreEmpty:"true" reportApplied:"true"`
	Domain   string `config:"domain" env:"CRABBOX_E2B_DOMAIN" envAlias:"E2B_DOMAIN" flag:"e2b-domain" sources:"user,repo,env,flag" help:"E2B sandbox domain" default:"e2b.app" fileIgnoreEmpty:"true" reportApplied:"true"`
	Template string `config:"template" env:"CRABBOX_E2B_TEMPLATE" flag:"e2b-template" sources:"user,repo,env,flag" help:"E2B sandbox template ID" default:"base" fileIgnoreEmpty:"true"`
	Workdir  string `config:"workdir" env:"CRABBOX_E2B_WORKDIR" flag:"e2b-workdir" sources:"user,repo,env,flag" help:"E2B sandbox working directory" default:"crabbox" fileIgnoreEmpty:"true"`
	User     string `config:"user" env:"CRABBOX_E2B_USER" flag:"e2b-user" sources:"user,repo,env,flag" help:"E2B sandbox user for command and file ownership" fileIgnoreEmpty:"true"`
}
