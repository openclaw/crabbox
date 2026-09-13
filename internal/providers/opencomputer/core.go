package opencomputer

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName    = "opencomputer"
	leasePrefix     = "ocbx_"
	namePrefix      = "crabbox-"
	defaultAPIURL   = "https://app.opencomputer.dev"
	targetLinux     = core.TargetLinux
	NetworkPublic   = core.NetworkPublic
	statusViewReady = "running"

	maxSandboxNameLen    = 63
	sandboxNameSuffixLen = 6
)

func listOpenComputerLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}
