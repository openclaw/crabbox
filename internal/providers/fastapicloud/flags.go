package fastapicloud

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// RegisterFastAPICloudProviderFlags exposes only non-secret provider flags.
// Deploy tokens are sourced from FASTAPI_CLOUD_TOKEN /
// CRABBOX_FASTAPI_CLOUD_TOKEN so they are not passed as command-line
// arguments.
func RegisterFastAPICloudProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterFastAPICloudConfigFlags(fs, defaults.FastAPICloud)
}

func ApplyFastAPICloudProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if isFastAPICloudProviderName(cfg.Provider) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "", ""); err != nil {
			return err
		}
	}
	v, ok := values.(core.FastAPICloudConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.FastAPICloud, fs)
	return nil
}

func isFastAPICloudProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "fastapicloud", "fastapi":
		return true
	default:
		return false
	}
}
