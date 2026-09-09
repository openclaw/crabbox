package cli

//go:generate go run ../../scripts/configgen -source config_upstash_box.go -output config_upstash_box_generated.go -type UpstashBoxConfig -provider upstash-box

// UpstashBoxConfig describes mechanical bindings for the delegated provider.
// Credential provenance and destination validation remain owned by core policy.
type UpstashBoxConfig struct {
	APIKey    string `env:"CRABBOX_UPSTASH_BOX_API_KEY" envAlias:"UPSTASH_BOX_API_KEY" sources:"env" reportApplied:"true"`
	BaseURL   string `config:"baseUrl" env:"CRABBOX_UPSTASH_BOX_BASE_URL" envAlias:"UPSTASH_BOX_BASE_URL" flag:"upstash-box-base-url" sources:"user,repo,env,flag" help:"Upstash Box API base URL" default:"https://us-east-1.box.upstash.com" fileIgnoreEmpty:"true" reportApplied:"true"`
	Runtime   string `config:"runtime" env:"CRABBOX_UPSTASH_BOX_RUNTIME" flag:"upstash-box-runtime" sources:"user,repo,env,flag" help:"Upstash Box runtime: node, python, golang, ruby, or rust" default:"node" fileIgnoreEmpty:"true"`
	Size      string `config:"size" env:"CRABBOX_UPSTASH_BOX_SIZE" flag:"upstash-box-size" sources:"user,repo,env,flag" help:"Upstash Box size: small, medium, or large" default:"small" fileIgnoreEmpty:"true"`
	Workdir   string `config:"workdir" env:"CRABBOX_UPSTASH_BOX_WORKDIR" flag:"upstash-box-workdir" sources:"user,repo,env,flag" help:"absolute working directory inside the Upstash Box" default:"/workspace/home/crabbox" fileIgnoreEmpty:"true"`
	KeepAlive bool   `config:"keepAlive" env:"CRABBOX_UPSTASH_BOX_KEEP_ALIVE" flag:"upstash-box-keep-alive" sources:"user,repo,env,flag" help:"create Upstash boxes with keepAlive enabled"`
}
