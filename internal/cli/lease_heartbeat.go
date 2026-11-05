package cli

import (
	"context"
	"fmt"
	"io"
	"time"
)

func startActionsLeaseHeartbeat(
	ctx context.Context,
	cancelOperation context.CancelCauseFunc,
	backend Backend,
	lease LeaseTarget,
	expectedClaim leaseClaim,
	cfg Config,
	stderr io.Writer,
) (func(), error) {
	if coord := backendCoordinator(backend); coord != nil {
		return startCoordinatorHeartbeat(
			ctx,
			coord,
			lease.LeaseID,
			cfg.Provider,
			leaseSavedIdleTimeout(cfg.IdleTimeout, leaseClaim{}, lease.Server.Labels),
			nil,
			leaseTelemetryCollectorForTarget(lease.SSH),
			stderr,
		)
	}
	if touchBackend, ok := backend.(LeaseTouchBackend); ok {
		return startDirectLeaseHeartbeat(
			ctx,
			guestExpiryHeartbeatBackend{LeaseTouchBackend: touchBackend, cfg: cfg},
			lease,
			expectedClaim,
			cancelOperation,
			blank(lease.Server.Labels["state"], "ready"),
			leaseSavedIdleTimeout(cfg.IdleTimeout, expectedClaim, lease.Server.Labels),
			stderr,
		), nil
	}
	return func() {}, nil
}

// leaseSavedIdleTimeout returns the idle timeout the lease itself carries.
// Renewals extend expiry by that value, so pacing them from the caller's
// config would let a lease warmed with a shorter idle timeout lapse between
// heartbeats. A recorded claim value wins, as it does for claim-backed touches.
func leaseSavedIdleTimeout(fallback time.Duration, claim leaseClaim, labels map[string]string) time.Duration {
	if claim.IdleTimeoutSeconds > 0 {
		return time.Duration(claim.IdleTimeoutSeconds) * time.Second
	}
	for _, candidate := range []map[string]string{claim.Labels, labels} {
		if saved, ok := directLeaseIdleTimeout(candidate); ok && saved > 0 {
			return saved
		}
	}
	return fallback
}

func waitForHeartbeatTick(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// guestExpiryHeartbeatBackend routes heartbeat touches through the shared touch
// owner so file-backed guest expiry guards advance with the lease.
type guestExpiryHeartbeatBackend struct {
	LeaseTouchBackend
	cfg Config
}

func (b guestExpiryHeartbeatBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	return touchLeaseWithGuestExpiry(contextWithoutWorkspaceOwner(ctx), b.LeaseTouchBackend, b.cfg, req)
}

func startDirectLeaseHeartbeat(
	ctx context.Context,
	backend LeaseTouchBackend,
	lease LeaseTarget,
	expectedClaim leaseClaim,
	cancelOperation context.CancelCauseFunc,
	state string,
	idleTimeout time.Duration,
	stderr io.Writer,
) func() {
	return startDirectLeaseHeartbeatPaced(ctx, backend, lease, expectedClaim, cancelOperation, state, idleTimeout, 0, waitForHeartbeatTick, stderr)
}

func startDirectLeaseHeartbeatWithInterval(
	ctx context.Context,
	backend LeaseTouchBackend,
	lease LeaseTarget,
	expectedClaim leaseClaim,
	cancelOperation context.CancelCauseFunc,
	state string,
	idleTimeout, interval time.Duration,
	stderr io.Writer,
) func() {
	if interval <= 0 {
		interval = time.Minute
	}
	return startDirectLeaseHeartbeatPaced(ctx, backend, lease, expectedClaim, cancelOperation, state, idleTimeout, interval, waitForHeartbeatTick, stderr)
}

// startDirectLeaseHeartbeatPaced renews the lease until stopped. A zero
// interval re-derives the pace after every renewal from the idle timeout the
// lease currently carries, so an extend that shortens it takes effect.
func startDirectLeaseHeartbeatPaced(
	ctx context.Context,
	backend LeaseTouchBackend,
	lease LeaseTarget,
	expectedClaim leaseClaim,
	cancelOperation context.CancelCauseFunc,
	state string,
	idleTimeout, interval time.Duration,
	wait func(context.Context, time.Duration) bool,
	stderr io.Writer,
) func() {
	rootCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var fatalErr, claimReadErr error
			var preTouch leaseClaim
			if expectedClaim.LeaseID != "" {
				current, exists, resolveErr := ResolveLeaseClaim(lease.LeaseID)
				switch {
				case resolveErr != nil:
					claimReadErr = resolveErr
				case !exists:
					fatalErr = Exit(4, "lease %s claim disappeared before heartbeat", lease.LeaseID)
				case !actionsHeartbeatClaimIdentityMatches(expectedClaim, current):
					fatalErr = Exit(4, "lease %s claim changed ownership before heartbeat", lease.LeaseID)
				default:
					lease.Server.Labels = withClaimLifetimePolicy(lease.Server.Labels, current.Labels, nil)
					// Renew against the claim just validated; a provider that commits
					// claim snapshots rejects a touch carrying an older one.
					SetServerLeaseClaimSnapshot(&lease.Server, current, true)
					preTouch = current
				}
			}
			if fatalErr != nil {
				if cancelOperation != nil {
					cancelOperation(fatalErr)
				}
				return
			}
			var server Server
			err := claimReadErr
			if claimReadErr == nil {
				callCtx, touchCancel := context.WithTimeout(rootCtx, 20*time.Second)
				server, err = backend.Touch(callCtx, TouchRequest{
					Lease:       lease,
					State:       state,
					IdleTimeout: leaseSavedIdleTimeout(idleTimeout, preTouch, lease.Server.Labels),
				})
				touchCancel()
			}
			if claimReadErr != nil {
				// An unreadable claim cannot fence this renewal; skip the touch and retry next tick.
			} else if err == nil {
				lease.Server = server
				if expectedClaim.LeaseID != "" {
					current, exists, resolveErr := ResolveLeaseClaim(lease.LeaseID)
					switch {
					case resolveErr != nil:
						err = resolveErr
					case !exists:
						fatalErr = Exit(4, "lease %s claim disappeared during heartbeat", lease.LeaseID)
						err = fatalErr
					case !actionsHeartbeatClaimAttests(expectedClaim, current, server):
						fatalErr = Exit(4, "lease %s claim changed ownership during heartbeat", lease.LeaseID)
						err = fatalErr
					default:
						server.Labels = withClaimLifetimePolicy(server.Labels, current.Labels, &preTouch)
						var updateErr error
						expectedClaim, updateErr, fatalErr = refreshActionsHeartbeatClaim(
							rootCtx,
							lease.LeaseID,
							expectedClaim,
							current,
							server,
							time.Now().UTC(),
						)
						err = updateErr
						if fatalErr != nil {
							err = fatalErr
						} else {
							SetServerLeaseClaimSnapshot(&lease.Server, expectedClaim, true)
						}
					}
				}
			} else if expectedClaim.LeaseID != "" && rootCtx.Err() == nil {
				// A rejected touch may mean another writer advanced the claim; reload it so the next tick carries a current snapshot.
				current, exists, resolveErr := ResolveLeaseClaim(lease.LeaseID)
				switch {
				case resolveErr != nil:
				case !exists:
					fatalErr = Exit(4, "lease %s claim disappeared during heartbeat", lease.LeaseID)
				case !actionsHeartbeatClaimAttests(expectedClaim, current, lease.Server):
					fatalErr = Exit(4, "lease %s claim changed ownership during heartbeat", lease.LeaseID)
				default:
					expectedClaim = current
					SetServerLeaseClaimSnapshot(&lease.Server, current, true)
				}
			}
			if err != nil && rootCtx.Err() == nil {
				fmt.Fprintf(stderr, "warning: direct lease heartbeat failed for %s: %v\n", lease.LeaseID, err)
			}
			if fatalErr != nil {
				if cancelOperation != nil {
					cancelOperation(fatalErr)
				}
				return
			}
			next := interval
			if next <= 0 {
				next = heartbeatInterval(leaseSavedIdleTimeout(idleTimeout, expectedClaim, lease.Server.Labels))
			}
			if !wait(rootCtx, next) {
				return
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func refreshActionsHeartbeatClaim(
	ctx context.Context,
	leaseID string,
	expected, current leaseClaim,
	server Server,
	lastUsed time.Time,
) (leaseClaim, error, error) {
	publishCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	updated, err := UpdateLeaseClaimTouchIfUnchanged(publishCtx, leaseID, current, server.Labels, lastUsed, nil)
	if err == nil {
		return updated, nil, nil
	}
	latest, exists, resolveErr := ResolveLeaseClaim(leaseID)
	if resolveErr != nil {
		return expected, fmt.Errorf("update lease claim: %v; reload lease claim: %w", err, resolveErr), nil
	}
	if !exists {
		return expected, err, Exit(4, "lease %s claim disappeared after heartbeat update failed", leaseID)
	}
	if !actionsHeartbeatClaimAttests(expected, latest, server) {
		return expected, err, Exit(4, "lease %s claim changed ownership after heartbeat update failed", leaseID)
	}
	return latest, err, nil
}

func actionsHeartbeatClaimAttests(expected, current leaseClaim, server Server) bool {
	return actionsHeartbeatClaimIdentityMatches(expected, current) &&
		resolvedLeaseClaimAttestsResult(current, server, expected.RepoRoot, expected.ProviderScope)
}

func actionsHeartbeatClaimIdentityMatches(expected, current leaseClaim) bool {
	if expected.LeaseID != current.LeaseID ||
		canonicalClaimProvider(expected.Provider) != canonicalClaimProvider(current.Provider) ||
		expected.ProviderScope != current.ProviderScope ||
		expected.RepoRoot != current.RepoRoot ||
		expected.Pond != current.Pond {
		return false
	}
	if expected.Slug != "" && expected.Slug != current.Slug {
		return false
	}
	if expected.CloudID != "" && expected.CloudID != current.CloudID {
		return false
	}
	if expected.CloudNumericID != 0 && expected.CloudNumericID != current.CloudNumericID {
		return false
	}
	if expected.CloudImmutableID != "" && expected.CloudImmutableID != current.CloudImmutableID {
		return false
	}
	if expected.StaticHost != "" && expected.StaticHost != current.StaticHost {
		return false
	}
	return true
}

func actionsOperationCause(ctx context.Context) error {
	cause := context.Cause(ctx)
	if cause == nil || cause == context.Canceled {
		return nil
	}
	return cause
}

// withClaimLifetimePolicy carries lifetime labels that an extend published to the claim into the next touch.
func withClaimLifetimePolicy(labels, claimLabels map[string]string, changedSince *leaseClaim) map[string]string {
	next := cloneStringMap(labels)
	if next == nil {
		next = map[string]string{}
	}
	for _, key := range []string{"ttl_secs", "max_ttl_secs", "idle_timeout", "idle_timeout_secs", "expires_at"} {
		if value, ok := claimLabels[key]; ok && value != "" {
			// After a touch only a claim value that moved since the pre-touch read is a concurrent extension; an unchanged one is stale next to the touch result.
			if changedSince != nil && changedSince.Labels[key] == value {
				continue
			}
			next[key] = value
		}
	}
	return next
}
