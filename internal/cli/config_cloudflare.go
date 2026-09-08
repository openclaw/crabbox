package cli

//go:generate go run ../../scripts/configgen -source config_cloudflare.go -output config_cloudflare_generated.go -type CloudflareConfig -provider cloudflare

// CloudflareConfig describes mechanical bindings for the container runner.
// Credential provenance and destination validation remain owned by core policy.
type CloudflareConfig struct {
	APIURL  string `config:"apiUrl" env:"CRABBOX_CLOUDFLARE_RUNNER_URL" flag:"cloudflare-url" sources:"user,repo,env,flag" help:"Cloudflare runner API URL" fileIgnoreEmpty:"true" reportApplied:"true"`
	Token   string `config:"token" env:"CRABBOX_CLOUDFLARE_RUNNER_TOKEN" sources:"user,repo,env" fileIgnoreEmpty:"true" reportApplied:"true"`
	Workdir string `config:"workdir" env:"CRABBOX_CLOUDFLARE_WORKDIR" flag:"cloudflare-workdir" sources:"user,repo,env,flag" help:"Absolute working directory inside the Cloudflare workspace" default:"/workspace/crabbox" fileIgnoreEmpty:"true"`
}
