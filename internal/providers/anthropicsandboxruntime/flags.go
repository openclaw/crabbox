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
	if ok, err := core.ApplyProviderConfigFlags[core.AnthropicSRTConfigFlagValues](cfg, fs, values, &cfg.AnthropicSRT, providerName); !ok || err != nil {
		return err
	}
	return validateConfig(*cfg)
}

func validateConfig(cfg core.Config) error {
	if strings.TrimSpace(cfg.AnthropicSRT.CLIPath) == "" {
		return core.Exit(2, "anthropicSandboxRuntime cliPath must not be empty")
	}
	return nil
}
