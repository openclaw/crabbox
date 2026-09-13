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
	applied, err := v.Apply(&cfg.AnthropicSRT, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if err != nil {
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
