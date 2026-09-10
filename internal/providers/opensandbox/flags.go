package opensandbox

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterOpenSandboxProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterOpenSandboxConfigFlags(fs, defaults.OpenSandbox)
}

func ApplyOpenSandboxProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case providerName:
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --opensandbox-cpu and --opensandbox-memory", "use --opensandbox-cpu and --opensandbox-memory"); err != nil {
			return err
		}
	}
	v, ok := values.(core.OpenSandboxConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.OpenSandbox, fs)
	return validateOpenSandboxConfig(*cfg)
}
