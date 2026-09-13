package blaxel

import (
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName      = "blaxel"
	defaultAPIVersion = "2026-04-28"
	targetLinux       = core.TargetLinux
	networkPublic     = core.NetworkPublic
	leasePrefix       = "blx_"
	recoveryPrefix    = "blxr_"
	namePrefix        = "crabbox-"
	sandboxNameMaxLen = 49

	blaxelCleanupTimeout = 15 * time.Second
	blaxelReadyTimeout   = 5 * time.Minute
	blaxelStatusPoll     = 2 * time.Second
	blaxelClaimKey       = "crabbox.claim"
)

func listBlaxelLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}

func listBlaxelCleanupClaims() ([]core.LeaseClaim, error) {
	claims, err := listBlaxelLeaseClaims()
	if err != nil {
		return nil, err
	}
	recoveries, err := core.ListLeaseClaimsWithPrefix(recoveryPrefix)
	if err != nil {
		return nil, err
	}
	return append(claims, recoveries...), nil
}

func now(rt core.Runtime) time.Time {
	if rt.Clock != nil {
		return rt.Clock.Now()
	}
	return time.Now()
}
