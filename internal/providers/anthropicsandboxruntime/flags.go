package anthropicsandboxruntime

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	CLIPath  *string
	Settings *string
	Debug    *bool
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		CLIPath:  fs.String("anthropic-sandbox-runtime-cli", defaults.AnthropicSRT.CLIPath, "path to the srt CLI binary"),
		Settings: fs.String("anthropic-sandbox-runtime-settings", defaults.AnthropicSRT.Settings, "path to an Anthropic Sandbox Runtime settings JSON file; empty uses srt defaults"),
		Debug:    fs.Bool("anthropic-sandbox-runtime-debug", defaults.AnthropicSRT.Debug, "pass --debug to the srt CLI"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "anthropic-sandbox-runtime-cli") {
		cfg.AnthropicSRT.CLIPath = *v.CLIPath
	}
	if core.FlagWasSet(fs, "anthropic-sandbox-runtime-settings") {
		cfg.AnthropicSRT.Settings = *v.Settings
	}
	if core.FlagWasSet(fs, "anthropic-sandbox-runtime-debug") {
		cfg.AnthropicSRT.Debug = *v.Debug
	}
	return validateConfig(*cfg)
}

func validateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.AnthropicSRT.CLIPath) == "" {
		return exit(2, "anthropicSandboxRuntime cliPath must not be empty")
	}
	return nil
}
