package tensorlake

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName  = "tensorlake"
	leasePrefix   = "tlsbx_"
	namePrefix    = "crabbox-"
	targetLinux   = core.TargetLinux
	NetworkPublic = core.NetworkPublic

	maxSandboxNameLen    = 63
	sandboxNameSuffixLen = 6
)

func tensorlakeCleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " --id " + core.ShellQuote(leaseID)
}
