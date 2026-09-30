package cloudflare

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// basicInstanceTypeWarning explains the one accepted legacy type: the
// durable_object scheduling policy has no basic, and standard-1 is larger.
const basicInstanceTypeWarning = "warning: cloudflare --type basic is deprecated; using standard-1 (1/2 vCPU, 4 GiB, 8 GB disk instead of 1/4 vCPU, 1 GiB, 4 GB), which costs more per second"

func resolveInstanceType(candidate, fallback string, explicit bool) (string, error) {
	if normalized, ok := normalizeContainerInstanceType(candidate); ok {
		return normalized, nil
	}
	if explicit {
		if strings.EqualFold(strings.TrimSpace(candidate), "lite") {
			return "", core.Exit(2, "%s --type lite cannot start the bundled runner image; use standard-1", providerName)
		}
		return "", core.Exit(2, "%s --type must be one of %s", providerName, strings.Join(containerInstanceTypes(), ", "))
	}
	return fallback, nil
}

func containerInstanceTypes() []string {
	return []string{"standard-1", "standard-2", "standard-3", "standard-4"}
}

func isBasicInstanceType(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "basic")
}

func normalizeContainerInstanceType(value string) (string, bool) {
	if isBasicInstanceType(value) {
		return "standard-1", true
	}
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
