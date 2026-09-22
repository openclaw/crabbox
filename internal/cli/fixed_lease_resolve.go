package cli

import (
	"context"
	"maps"
	"reflect"
	"strconv"
	"time"
)

// FixedResolveOptions supplies routing facts and the existing terminal dialect.
// Native callbacks prepare access or inspect evidence; core owns the claim CAS,
// read-only boundary, checkpoint admission and acquired publication.
type FixedResolveOptions struct {
	Kind                                  FixedLeaseKind
	Request                               ResolveRequest
	Expected                              LeaseClaim
	Provider, ResourceName, TerminalState string
	ValidateTerminal                      func(LeaseClaim) error
	Now                                   func() time.Time
}

func ResolveFixedLeaseTarget(ctx context.Context, opts FixedResolveOptions,
	prepare func(context.Context, *LeaseClaim, func() error) (LeaseTarget, error),
	inspect func(context.Context, LeaseClaim) (LeaseTarget, error),
) (LeaseTarget, error) {
	var lease LeaseTarget
	req := opts.Request
	err := WithDurableLeaseClaimLockContext(ctx, opts.Expected.LeaseID, func(claim *LeaseClaim, exists bool, persist func() error) error {
		if !exists || !reflect.DeepEqual(*claim, opts.Expected) {
			return Exit(4, "lease_id_conflict: fixed claim changed during resolution; retry")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if req.Repo.Root != "" && claim.RepoRoot != req.Repo.Root {
			return Exit(4, "lease_id_conflict: fixed lease %s belongs to another repository", claim.LeaseID)
		}
		if claim.FixedCreateIntent != nil && claim.FixedCreateIntent.State == "released" {
			if err := opts.Kind.ValidateTerminalClaim(*claim, LeaseClaim{}, claim.LeaseID, opts.ValidateTerminal); err != nil {
				return err
			}
			if !req.StatusOnly && !req.ReleaseOnly {
				return Exit(4, "lease_id_conflict: fixed lease %s is terminal", claim.LeaseID)
			}
			labels := maps.Clone(claim.Labels)
			if labels == nil {
				labels = map[string]string{}
			}
			labels["lease"], labels["slug"], labels["provider"], labels["state"] = claim.LeaseID, claim.Slug, opts.Provider, opts.TerminalState
			lease = LeaseTarget{LeaseID: claim.LeaseID, Server: Server{CloudID: claim.CloudID, ImmutableID: claim.CloudImmutableID, Provider: opts.Provider, Name: opts.ResourceName, Status: opts.TerminalState, Labels: labels}}
		} else if !req.StatusOnly && !req.ReleaseOnly {
			if req.NoLocalStateMutations {
				return Exit(4, "fixed command preparation requires durable identity binding")
			}
			if err := AuthorizeCheckpointRelease(*claim, ""); err != nil {
				return err
			}
			var err error
			lease, err = prepare(ctx, claim, persist)
			if err != nil {
				return err
			}
			claim.Labels, claim.SSHHost = maps.Clone(lease.Server.Labels), lease.SSH.Host
			claim.SSHPort, _ = strconv.Atoi(lease.SSH.Port)
			now := opts.Now
			if now == nil {
				now = time.Now
			}
			claim.LastUsedAt = now().UTC().Format(time.RFC3339)
			claim.FixedCreateIntent.State = "acquired"
			if err := persist(); err != nil {
				return err
			}
		} else {
			var err error
			lease, err = inspect(ctx, *claim)
			if err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		SetServerLeaseClaimSnapshot(&lease.Server, *claim, true)
		return nil
	})
	return lease, err
}

// ReclaimFixedLease transfers an attested resource under the original claim CAS.
// Native lookup must not mutate the resource or local ownership record.
func ReclaimFixedLease(ctx context.Context, claim LeaseClaim, cfg Config, repoRoot string, reclaim bool, lookup func(context.Context, LeaseClaim) (Server, error)) (LeaseClaim, error) {
	if !reclaim {
		return claim, nil
	}
	if repoRoot == "" {
		return claim, Exit(2, "fixed reclaim requires the current repository")
	}
	if err := AuthorizeCheckpointRelease(claim, ""); err != nil {
		return claim, err
	}
	server, err := lookup(ctx, claim)
	if err != nil {
		return claim, err
	}
	return ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfterContext(ctx, claim.LeaseID, claim.Slug, cfg, claim.ProviderScope, server, SSHTarget{}, repoRoot, cfg.IdleTimeout, true, claim, true,
		func() error { return AuthorizeCheckpointRelease(claim, "") })
}
