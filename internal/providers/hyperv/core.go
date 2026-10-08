package hyperv

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName  = "hyperv"
	targetLinux   = core.TargetLinux
	targetWindows = core.TargetWindows
	sshPort       = "22"
)

const (
	secureBootAuto    = "auto"
	secureBootWindows = "windows"
	secureBootLinux   = "linux"
	secureBootOff     = "off"
)

func updateLeaseClaimEndpointIfUnchanged(leaseID string, expected core.LeaseClaim, server core.Server, target core.SSHTarget) error {
	updated, err := core.UpdateLeaseClaimEndpointIfUnchanged(leaseID, expected, server, target)
	if err != nil {
		return err
	}
	if updated.LeaseID != leaseID || updated.SSHHost != target.Host {
		return core.Exit(4, "hyperv lease %q endpoint refresh did not persist host %q", leaseID, target.Host)
	}
	return nil
}
