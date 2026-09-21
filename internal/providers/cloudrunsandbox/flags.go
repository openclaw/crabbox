package cloudrunsandbox

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterCloudRunSandboxProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterCloudRunSandboxConfigFlags(fs, defaults.CloudRunSandbox)
}

func ApplyCloudRunSandboxProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "sandboxes share Cloud Run service CPU/memory", "sandboxes share Cloud Run service CPU/memory"); err != nil {
			return err
		}
	}
	if ok, err := core.ApplyProviderConfigFlags[core.CloudRunSandboxConfigFlagValues](cfg, fs, values, &cfg.CloudRunSandbox, providerName); !ok || err != nil {
		return err
	}
	return validateConfig(*cfg)
}
