package cloudflare

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func resolveInstanceType(candidate, fallback string, explicit bool) (string, error) {
	if normalized, ok := normalizeContainerInstanceType(candidate); ok {
		return normalized, nil
	}
	if explicit {
		return "", core.Exit(2, "%s --type must be one of %s", providerName, strings.Join(containerInstanceTypes(), ", "))
	}
	return fallback, nil
}

func containerInstanceTypes() []string {
	return []string{"lite", "basic", "standard-1", "standard-2", "standard-3", "standard-4"}
}

func normalizeContainerInstanceType(value string) (string, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	for _, instanceType := range containerInstanceTypes() {
		if trimmed == instanceType {
			return instanceType, true
		}
	}
	return "", false
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
