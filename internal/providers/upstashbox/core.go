package upstashbox

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName = "upstash-box"
	targetLinux  = core.TargetLinux

	networkPublic = core.NetworkPublic
)

func upstashBoxCleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " " + core.ShellQuote(leaseID)
}
