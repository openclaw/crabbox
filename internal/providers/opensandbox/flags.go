package opensandbox

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func RegisterOpenSandboxProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterOpenSandboxConfigFlags(fs, defaults.OpenSandbox)
}

func ApplyOpenSandboxProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case providerName:
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=opensandbox; use --opensandbox-cpu and --opensandbox-memory")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=opensandbox; use --opensandbox-cpu and --opensandbox-memory")
		}
	}
	v, ok := values.(core.OpenSandboxConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.OpenSandbox, fs)
	return validateOpenSandboxConfig(*cfg)
}
