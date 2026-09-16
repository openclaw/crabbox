package superserve

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName   = "superserve"
	leasePrefix    = "ssbx_"
	namePrefix     = "crabbox-"
	defaultBaseURL = "https://api.superserve.ai"
	defaultWorkdir = "/workspace/crabbox"
	targetLinux    = core.TargetLinux
)

func listSuperserveLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}

func superserveCleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " --id " + core.ShellQuote(leaseID)
}
