package cloudflare

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func RegisterCloudflareProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterCloudflareConfigFlags(fs, defaults.Cloudflare)
}

func ApplyCloudflareProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if isCloudflareProviderName(cfg.Provider) {
		instanceType := strings.TrimSpace(cfg.ServerType)
		if instanceType == "" {
			instanceType = cloudflareContainerInstanceTypeForClass(cfg.Class)
		}
		normalized, ok := normalizeCloudflareContainerInstanceType(instanceType)
		if !ok {
			if flagWasSet(fs, "type") || cfg.ServerTypeExplicit {
				return exit(2, "%s --type must be one of %s", providerName, strings.Join(cloudflareContainerInstanceTypes(), ", "))
			}
			normalized = cloudflareContainerInstanceTypeForClass(cfg.Class)
		}
		cfg.ServerType = normalized
		cfg.ServerTypeExplicit = flagWasSet(fs, "type") || cfg.ServerTypeExplicit
	}
	v, ok := values.(core.CloudflareConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.Cloudflare, fs)
	return nil
}

func isCloudflareProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, providerAlias:
		return true
	default:
		return false
	}
}
