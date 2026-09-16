package crownest

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName = "crownest"
	leasePrefix  = "cnsbx_"
	targetLinux  = core.TargetLinux
)

func listCrownestLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}
