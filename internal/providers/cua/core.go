package cua

import (
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName            = "cua"
	targetLinux             = core.TargetLinux
	cuaTrackingIssue        = "https://github.com/openclaw/crabbox/issues/381"
	maxBridgeTimeoutSeconds = int64((1<<63 - 1) / int64(time.Second))
)

func provisioningUnsupported() error {
	return core.Exit(2, "provider=cua provisioning is disabled: the upstream CUA create API has no idempotency key or client-assigned identity echoed by create/list/get, so a timed-out create could orphan a billed sandbox; use doctor, list, or status; tracking issue: %s", cuaTrackingIssue)
}

func mutationUnsupported() error {
	return core.Exit(2, "provider=cua is experimental and read-only: remote mutation is disabled because upstream deletion cannot atomically bind to an immutable sandbox identity; use doctor, list, or status; tracking issue: %s", cuaTrackingIssue)
}

func listCUALeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}
