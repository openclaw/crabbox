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
		normalized, ok := core.NormalizeCloudflareContainerInstanceType(instanceType)
		if !ok {
			if core.FlagWasSet(fs, "type") || cfg.ServerTypeExplicit {
				return core.Exit(2, "%s --type must be one of %s", providerName, strings.Join(core.CloudflareContainerInstanceTypes(), ", "))
			}
			normalized = cloudflareContainerInstanceTypeForClass(cfg.Class)
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
