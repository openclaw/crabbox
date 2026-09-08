package codesandbox

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
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
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=codesandbox; use --codesandbox-vm-tier")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=codesandbox; use --codesandbox-vm-tier")
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
