package cloudflare

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func resolveInstanceType(candidate, fallback string, explicit bool) (string, error) {
	if normalized, ok := core.NormalizeCloudflareContainerInstanceType(candidate); ok {
		return normalized, nil
	}
	if explicit {
		return "", core.Exit(2, "%s --type must be one of %s", providerName, strings.Join(core.CloudflareContainerInstanceTypes(), ", "))
	}
	return fallback, nil
}

const (
	providerName  = "cloudflare"
	providerAlias = "cf"
	targetLinux   = core.TargetLinux
	networkPublic = core.NetworkPublic
)

func cloudflareContainerInstanceTypeForClass(class string) string {
	return (Provider{}).ServerTypeForConfig(core.Config{Provider: providerName, TargetOS: core.TargetLinux, Architecture: core.ArchitectureAMD64, Class: class})
}
