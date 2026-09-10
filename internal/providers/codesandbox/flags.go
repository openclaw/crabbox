package codesandbox

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterCodeSandboxProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterCodeSandboxConfigFlags(fs, defaults.CodeSandbox)
}

func ApplyCodeSandboxProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.CodeSandboxConfigFlagValues)
	if !ok {
		return nil
	}
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --codesandbox-vm-tier", "use --codesandbox-vm-tier"); err != nil {
			return err
		}
	}
	v.Apply(&cfg.CodeSandbox, fs)
	return validateCodeSandboxConfig(*cfg)
}
