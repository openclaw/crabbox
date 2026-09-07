package cli

import (
	"context"
	"errors"
	"time"
)

var errRunClaimAdmissionUnavailable = errors.New("run claim admission unavailable")

// Alias resolution, adoption and incomplete fixed acquisitions retain their
// existing owners. A bound canonical reuse owns one lock from the original
// claim read through provider preparation and publication, so a heartbeat
// cannot invalidate its own command admission halfway through that flow.
func admitRunLeaseUnderClaim(ctx context.Context, backend SSHLoginBackend, req ResolveRequest, cfg *Config, admit func(*LeaseTarget) error) (LeaseTarget, bool, error) {
	resolver, ok := backend.(RunLeaseClaimResolver)
	if !ok || req.Reclaim || !isCanonicalLeaseID(req.ID) {
		return LeaseTarget{}, false, nil
	}
	var original leaseClaim
	var lease LeaseTarget
	updated, err := transactLeaseClaim(req.ID, leaseClaimTransaction{
		context:  ctx,
		revision: claimRevisionAfterMutation,
		guard: func(current leaseClaim, exists bool) error {
			if !exists || current.CloudID == "" || current.Provider == "" {
				return errRunClaimAdmissionUnavailable
			}
			if canonicalClaimProvider(current.Provider) != canonicalClaimProvider(backend.Spec().Name) || (req.Options.ProviderScope != "" && current.ProviderScope != req.Options.ProviderScope) {
				return exit(2, "lease %s does not match the requested provider scope", req.ID)
			}
			if err := checkLeaseClaimRepositoryOwner(req.ID, current, req.Repo.Root, false); err != nil {
				return err
			}
			original = cloneLeaseClaim(current)
			return nil
		},
		action: func() (claimActionDecision, error) {
			var err error
			lease, err = resolver.ResolveRunLeaseUnderClaim(ctx, req, original)
			if err != nil {
				return claimActionContinue, err
			}
			if lease.LeaseID != original.LeaseID || lease.Server.CloudID != original.CloudID || canonicalClaimProvider(lease.Server.Provider) != canonicalClaimProvider(original.Provider) {
				return claimActionContinue, exit(2, "lease %s resolved outside its original claim", req.ID)
			}
			if err := ctx.Err(); err != nil {
				return claimActionContinue, err
			}
			SetServerLeaseClaimSnapshot(&lease.Server, original, true)
			return claimActionContinue, admit(&lease)
		},
		mutate: func(current *leaseClaim) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			provider, details := claimProviderDetailsForConfig(*cfg)
			return transformLeaseClaimForRepo(current, req.ID, serverSlug(lease.Server), provider, providerClaimScope(provider, *cfg), cfg.Pond, details, req.Repo.Root, cfg.IdleTimeout, false, claimMetadata{
				setCacheVolumes: true,
				cacheVolumes:    CacheVolumeStickyDiskSpecs(cfg.Cache.Volumes),
				setEndpoint:     true,
				server:          lease.Server,
				target:          lease.SSH,
			}, time.Now().UTC().Format(time.RFC3339))
		},
	})
	if errors.Is(err, errRunClaimAdmissionUnavailable) {
		return LeaseTarget{}, false, nil
	}
	if err != nil {
		return LeaseTarget{}, true, err
	}
	SetServerLeaseClaimSnapshot(&lease.Server, updated, true)
	return lease, true, nil
}
