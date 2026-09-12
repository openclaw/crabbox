package cli

//go:generate go run ../../scripts/configgen -source config_anthropic_sandbox_runtime.go -output config_anthropic_sandbox_runtime_generated.go -type AnthropicSRTConfig -provider anthropic-sandbox-runtime

// AnthropicSRTConfig describes mechanical bindings for the local SRT provider.
// Native settings validation and sandbox policy remain owned by SRT.
type AnthropicSRTConfig struct {
	CLIPath  string `config:"cliPath" env:"CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_CLI" flag:"anthropic-sandbox-runtime-cli" sources:"user,repo,env,flag" help:"path to the srt CLI binary" default:"srt" fileIgnoreEmpty:"true" fileStorage:"value"`
	Settings string `config:"settings" env:"CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_SETTINGS" flag:"anthropic-sandbox-runtime-settings" sources:"user,repo,env,flag" help:"path to an Anthropic Sandbox Runtime settings JSON file; empty uses srt defaults"`
	Debug    bool   `config:"debug" env:"CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_DEBUG" flag:"anthropic-sandbox-runtime-debug" sources:"user,repo,env,flag" help:"pass --debug to the srt CLI"`
}
