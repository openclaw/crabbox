package cloudflare

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func RegisterCloudflareProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterCloudflareConfigFlags(fs, defaults.Cloudflare)
}

func ApplyCloudflareProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		instanceType := strings.TrimSpace(cfg.ServerType)
		if instanceType == "" {
			instanceType = cloudflareContainerInstanceTypeForClass(cfg.Class)
		}
		normalized, err := resolveInstanceType(instanceType, cloudflareContainerInstanceTypeForClass(cfg.Class), core.FlagWasSet(fs, "type") || cfg.ServerTypeExplicit)
		if err != nil {
			return err
		}
		cfg.ServerType = normalized
		cfg.ServerTypeExplicit = core.FlagWasSet(fs, "type") || cfg.ServerTypeExplicit
	}
	v, ok := values.(core.CloudflareConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Cloudflare, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	return err
}
