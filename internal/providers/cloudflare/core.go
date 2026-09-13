package cloudflare

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName  = "cloudflare"
	providerAlias = "cf"
	targetLinux   = core.TargetLinux
	networkPublic = core.NetworkPublic
)

func cloudflareContainerInstanceTypeForClass(class string) string {
	return (Provider{}).ServerTypeForClass(class)
}
