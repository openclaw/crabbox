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
			cfg.IdleTimeout,
			nil,
			leaseTelemetryCollectorForTarget(lease.SSH),
			stderr,
		)
	}
	if touchBackend, ok := backend.(LeaseTouchBackend); ok {
		return startDirectLeaseHeartbeat(
			ctx,
			touchBackend,
			lease,
			expectedClaim,
			cancelOperation,
			blank(lease.Server.Labels["state"], "ready"),
			cfg.IdleTimeout,
			stderr,
		), nil
	}
	return func() {}, nil
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
	return startDirectLeaseHeartbeatWithInterval(ctx, backend, lease, expectedClaim, cancelOperation, state, idleTimeout, heartbeatInterval(idleTimeout), stderr)
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
	rootCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			var fatalErr error
			callCtx, touchCancel := context.WithTimeout(rootCtx, 20*time.Second)
			server, err := backend.Touch(callCtx, TouchRequest{
				Lease:       lease,
				State:       state,
				IdleTimeout: idleTimeout,
			})
			touchCancel()
			if err == nil {
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
						var updateErr error
						expectedClaim, updateErr, fatalErr = refreshActionsHeartbeatClaim(
							lease.LeaseID,
							expectedClaim,
							current,
							server,
							time.Now().UTC(),
						)
						err = updateErr
						if fatalErr != nil {
							err = fatalErr
						}
					}
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
			select {
			case <-ticker.C:
			case <-rootCtx.Done():
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
	leaseID string,
	expected, current leaseClaim,
	server Server,
	lastUsed time.Time,
) (leaseClaim, error, error) {
	updated, err := UpdateLeaseClaimLabelsAndLastUsedIfUnchanged(
		leaseID,
		current,
		server.Labels,
		lastUsed,
	)
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
	return resolvedLeaseClaimAttestsResult(current, server, expected.RepoRoot, expected.ProviderScope)
}

func actionsOperationCause(ctx context.Context) error {
	cause := context.Cause(ctx)
	if cause == nil || cause == context.Canceled {
		return nil
	}
	return cause
}
