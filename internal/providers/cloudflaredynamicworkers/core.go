package cloudflaredynamicworkers

import (
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName             = "cloudflare-dynamic-workers"
	targetWorker             = core.TargetWorkerRuntime
	defaultCompatibilityDate = core.DefaultCloudflareDynamicWorkersCompatibilityDate
)

func claimLease(leaseID, slug string, cfg core.Config, repoRoot string, idleTimeout time.Duration, reclaim bool, server core.Server) error {
	scope, err := loaderClaimScope(cfg)
	if err != nil {
		return err
	}
	return core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, scope, cfg.Pond, repoRoot, idleTimeout, reclaim, server, core.SSHTarget{TargetOS: targetWorker})
}

func resolveLeaseClaim(identifier string, cfg core.Config) (core.LeaseClaim, bool, error) {
	scope, err := loaderClaimScope(cfg)
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	if identifier != "" {
		exact, exists, err := core.ReadLeaseClaimWithPresence(identifier)
		if err != nil {
			return core.LeaseClaim{}, false, err
		}
		if exists {
			if exact.Provider == providerName && exact.ProviderScope == scope {
				return exact, true, nil
			}
			if exact.Provider == providerName {
				return core.LeaseClaim{}, false, core.Exit(2, "%s claim %s belongs to a different loader endpoint", providerName, identifier)
			}
		}
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	var matched core.LeaseClaim
	for _, claim := range claims {
		if claim.Provider != providerName || claim.ProviderScope != scope {
			continue
		}
		if claim.LeaseID == identifier {
			return claim, true, nil
		}
		if matched.LeaseID == "" && core.LeaseClaimMatchesIdentifier(claim, identifier) {
			matched = claim
		}
	}
	return matched, matched.LeaseID != "", nil
}

func timingReportWithProviderError(report core.TimingReport) core.TimingReport {
	report.RunStatus = core.RunStatusFailed
	report.ErrorKind = core.RunErrorProvider
	return report
}
