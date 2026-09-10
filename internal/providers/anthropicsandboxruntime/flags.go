package anthropicsandboxruntime

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterAnthropicSRTConfigFlags(fs, defaults.AnthropicSRT)
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.AnthropicSRTConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.AnthropicSRT, fs)
	return validateConfig(*cfg)
}

func validateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.AnthropicSRT.CLIPath) == "" {
		return exit(2, "anthropicSandboxRuntime cliPath must not be empty")
	}
	return nil
}
