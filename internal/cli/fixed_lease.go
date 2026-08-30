package cli

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// Provider claim locks end before CLI registration and fork preparation. Keep
// same-ID callers serialized through that publication without nesting claim locks.
// Never unlink this lock: a waiter must keep using the same inode after unlock.
func lockFixedLeaseAcquisition(ctx context.Context, leaseID string) (func(), error) {
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		return nil, err
	}
	lockPath, err := leaseClaimLockPath(path + ".acquire")
	if err != nil {
		return nil, err
	}
	lock := flock.New(lockPath, flock.SetPermissions(0o600))
	if _, err := lock.TryLockContext(ctx, 100*time.Millisecond); err != nil {
		return nil, fmt.Errorf("wait for fixed lease %s acquisition: %w", leaseID, err)
	}
	return func() { _ = lock.Close() }, nil
}

type FixedLeaseBinding struct {
	ProviderScope string
	Fingerprint   string
	Slug          string
}

type FixedAcquireOptions struct {
	Kind         FixedLeaseKind
	LeaseID      string
	CheckpointID string
	RepoRoot     string
	Reclaim      bool
	TargetOS     string
	WindowsMode  string
	TTL          time.Duration
	IdleTimeout  time.Duration
	Now          func() time.Time
}

func AcquireFixedLease(
	opts FixedAcquireOptions,
	prepare func(context.Context, *LeaseClaim, bool) (FixedLeaseBinding, error),
	acquire func(context.Context, *LeaseClaim, *FixedCreateIntent, func() error) (LeaseTarget, error),
	ctx context.Context,
) (LeaseTarget, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	var acquired LeaseTarget
	err := WithDurableLeaseClaimLock(opts.LeaseID, func(claim *LeaseClaim, exists bool, persist func() error) error {
		if exists && opts.Kind.IsFixedClaim(*claim) && claim.FixedCreateIntent.State == "released" {
			return exit(4, "lease_id_conflict: fixed lease %s is terminal and cannot be replayed", opts.LeaseID)
		}
		if exists && claim.FixedCreateIntent != nil && claim.FixedCreateIntent.CheckpointID != opts.CheckpointID {
			return exit(4, "lease_id_conflict: lease %s is bound to checkpoint %s, not checkpoint %s", opts.LeaseID, blank(claim.FixedCreateIntent.CheckpointID, "<none>"), blank(opts.CheckpointID, "<none>"))
		}
		binding, err := prepare(ctx, claim, exists)
		if err != nil {
			return err
		}
		if exists {
			if claim.FixedCreateIntent == nil ||
				claim.FixedCreateIntent.Version != opts.Kind.IntentVersion ||
				claim.FixedCreateIntent.Fingerprint != binding.Fingerprint ||
				claim.FixedCreateIntent.ProviderScope != binding.ProviderScope {
				return exit(4, "lease_id_conflict: lease %s is bound to another create intent", opts.LeaseID)
			}
			if claim.Provider != opts.Kind.ClaimProvider {
				return exit(4, "lease_id_conflict: lease %s is bound to provider=%s", opts.LeaseID, claim.Provider)
			}
			if claim.RepoRoot != "" && claim.RepoRoot != opts.RepoRoot && !opts.Reclaim {
				return exit(4, "lease_id_conflict: lease %s is bound to another repository", opts.LeaseID)
			}
		} else {
			current := now().UTC()
			claim.LeaseID = opts.LeaseID
			claim.Slug = binding.Slug
			claim.Provider = opts.Kind.ClaimProvider
			claim.ProviderScope = binding.ProviderScope
			claim.TargetOS = opts.TargetOS
			claim.WindowsMode = opts.WindowsMode
			claim.RepoRoot = opts.RepoRoot
			claim.ClaimedAt = current.Format(time.RFC3339)
			claim.LastUsedAt = claim.ClaimedAt
			claim.IdleTimeoutSeconds = int(opts.IdleTimeout.Seconds())
			claim.FixedCreateIntent = &FixedCreateIntent{
				Version:       opts.Kind.IntentVersion,
				Fingerprint:   binding.Fingerprint,
				ProviderScope: binding.ProviderScope,
				CheckpointID:  opts.CheckpointID,
				Slug:          binding.Slug,
				CreatedAt:     current.Format(time.RFC3339Nano),
				State:         "prepared",
			}
			if err := persist(); err != nil {
				return err
			}
		}

		intent := claim.FixedCreateIntent
		if intent.State != "prepared" && intent.State != "acquired" {
			return exit(4, "lease_id_conflict: fixed lease %s has invalid create state %q", opts.LeaseID, intent.State)
		}
		createdAt, err := time.Parse(time.RFC3339Nano, intent.CreatedAt)
		if err != nil {
			return exit(4, "lease_id_conflict: lease %s has invalid fixed create timestamp", opts.LeaseID)
		}
		if opts.TTL > 0 && now().UTC().After(createdAt.Add(opts.TTL)) {
			return exit(4, "lease_id_conflict: fixed create intent for lease %s has expired", opts.LeaseID)
		}

		acquired, err = acquire(ctx, claim, intent, persist)
		if err != nil {
			return err
		}
		claim.CloudID = acquired.Server.CloudID
		claim.CloudImmutableID = acquired.Server.ImmutableID
		claim.Slug = intent.Slug
		claim.Provider = opts.Kind.ClaimProvider
		claim.Labels = maps.Clone(acquired.Server.Labels)
		claim.SSHHost = acquired.SSH.Host
		claim.LastUsedAt = now().UTC().Format(time.RFC3339)
		if port, parseErr := strconv.Atoi(strings.TrimSpace(acquired.SSH.Port)); parseErr == nil {
			claim.SSHPort = port
		}
		intent.State = "acquired"
		if err := persist(); err != nil {
			return err
		}
		SetServerLeaseClaimSnapshot(&acquired.Server, *claim, true)
		return nil
	})
	if err != nil {
		return LeaseTarget{}, err
	}
	return acquired, nil
}
