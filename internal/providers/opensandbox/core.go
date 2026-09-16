package opensandbox

import (
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName    = "opensandbox"
	leasePrefix     = "osbx_"
	recoveryPrefix  = "osbxr_"
	namePrefix      = "crabbox-"
	defaultAPIURL   = "http://localhost:8080"
	defaultWorkdir  = "/workspace/crabbox"
	defaultImage    = "ubuntu:24.04"
	targetLinux     = core.TargetLinux
	NetworkPublic   = core.NetworkPublic
	statusViewReady = "running"

	openSandboxCleanupTimeout   = 15 * time.Second
	openSandboxReadyTimeout     = 5 * time.Minute
	openSandboxExecGrace        = 30 * time.Second
	openSandboxMinimumTTL       = 10 * time.Minute
	openSandboxStatusPoll       = 2 * time.Second
	openSandboxStatusProbe      = 5 * time.Second
	openSandboxInterruptTimeout = 5 * time.Second
	openSandboxExecTimeoutSecs  = 600
	openSandboxClaimKey         = "crabbox.claim"
	openSandboxNameKey          = "crabbox.name"
)

func listOpenSandboxLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}

func listOpenSandboxCleanupClaims() ([]core.LeaseClaim, error) {
	claims, err := listOpenSandboxLeaseClaims()
	if err != nil {
		return nil, err
	}
	recoveries, err := core.ListLeaseClaimsWithPrefix(recoveryPrefix)
	if err != nil {
		return nil, err
	}
	return append(claims, recoveries...), nil
}

func openSandboxCleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " " + core.ShellQuote(leaseID)
}
