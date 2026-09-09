package cloudrunsandbox

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterCloudRunSandboxProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterCloudRunSandboxConfigFlags(fs, defaults.CloudRunSandbox)
}

func ApplyCloudRunSandboxProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case providerName, "gcrun-sandbox", "google-cloud-run-sandbox", "cloudrun-sandbox":
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "sandboxes share Cloud Run service CPU/memory", "sandboxes share Cloud Run service CPU/memory"); err != nil {
			return err
		}
	}
	v, ok := values.(core.CloudRunSandboxConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.CloudRunSandbox, fs)
	return validateConfig(*cfg)
}
