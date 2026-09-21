package opensandbox

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterOpenSandboxProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterOpenSandboxConfigFlags(fs, defaults.OpenSandbox)
}

func ApplyOpenSandboxProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case providerName:
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --opensandbox-cpu and --opensandbox-memory", "use --opensandbox-cpu and --opensandbox-memory"); err != nil {
			return err
		}
	}
	if ok, err := core.ApplyProviderConfigFlags[core.OpenSandboxConfigFlagValues](cfg, fs, values, &cfg.OpenSandbox, providerName); !ok || err != nil {
		return err
	}
	return validateOpenSandboxConfig(*cfg)
}
