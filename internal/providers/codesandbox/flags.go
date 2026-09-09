package codesandbox

import (
	"flag"
	"strings"

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
	if codeSandboxProviderSelected(cfg.Provider) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --codesandbox-vm-tier", "use --codesandbox-vm-tier"); err != nil {
			return err
		}
	}
	v.Apply(&cfg.CodeSandbox, fs)
	return validateCodeSandboxConfig(*cfg)
}

func codeSandboxProviderSelected(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "csb", "code-sandbox":
		return true
	default:
		return false
	}
}
