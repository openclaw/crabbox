package wandb

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName  = "wandb"
	targetLinux   = core.TargetLinux
	networkPublic = core.NetworkPublic
)

func claimWandbSandbox(leaseID, scope string, cfg core.Config) (core.LeaseClaim, error) {
	server := core.Server{CloudID: leaseID, Provider: providerName, Name: leaseID}
	return core.ClaimLeaseTargetForConfigScopeIfUnchanged(leaseID, leaseID, cfg, scope, server, core.SSHTarget{}, cfg.IdleTimeout, core.LeaseClaim{}, false)
}

func resolveWandbClaim(identifier string) (core.LeaseClaim, bool, error) {
	claim, ok, err := core.ResolveLeaseClaimForProvider(identifier, providerName)
	if err != nil || ok {
		return claim, ok, err
	}
	return core.ResolveLeaseClaimForProviderCloudID(identifier, providerName)
}

func removeWandbClaimAfter(claim core.LeaseClaim, action func() error) error {
	return core.RemoveLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, action)
}

func verifyWandbClaim(claim core.LeaseClaim) error {
	return core.VerifyLeaseClaimUnchanged(claim.LeaseID, claim)
}
