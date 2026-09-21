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
	_, err := core.ApplyProviderConfigFlags[core.CloudflareConfigFlagValues](cfg, fs, values, &cfg.Cloudflare, providerName)
	return err
}
