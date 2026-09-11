package cloudrunsandbox

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterCloudRunSandboxProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterCloudRunSandboxConfigFlags(fs, defaults.CloudRunSandbox)
}

func ApplyCloudRunSandboxProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "sandboxes share Cloud Run service CPU/memory", "sandboxes share Cloud Run service CPU/memory"); err != nil {
			return err
		}
	}
	v, ok := values.(core.CloudRunSandboxConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.CloudRunSandbox, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if err != nil {
		return err
	}
	return validateConfig(*cfg)
}
