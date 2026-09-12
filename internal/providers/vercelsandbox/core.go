package vercelsandbox

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName   = "vercel-sandbox"
	providerFamily = "vercel"
	leasePrefix    = "vsbx_"
	defaultWorkdir = core.VercelSandboxConfigDefaultWorkdir
	defaultRuntime = core.VercelSandboxConfigDefaultRuntime
	targetLinux    = core.TargetLinux
	NetworkPublic  = "public"
)

func listVercelSandboxLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}

func vercelSandboxCleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " " + core.ShellQuote(leaseID)
}
