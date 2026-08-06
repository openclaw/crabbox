package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var (
	coordinatorCreateLeaseProgressInterval   = 30 * time.Second
	coordinatorCreateLeaseTimeoutForConfig   = defaultCoordinatorCreateLeaseTimeoutForConfig
	coordinatorCreateLeaseRecoveryTimeout    = 90 * time.Second
	coordinatorCreateLeaseRecoveryInterval   = 5 * time.Second
	coordinatorCanceledCreateRecoveryTimeout = 10 * time.Second
	coordinatorCanceledCreateFinalTimeout    = 10 * time.Second
)

type coordinatorLeaseBackend struct {
	spec   ProviderSpec
	cfg    Config
	direct SSHLeaseBackend
	coord  *CoordinatorClient
	rt     Runtime
}

func (b *coordinatorLeaseBackend) Spec() ProviderSpec { return b.spec }

func (b *coordinatorLeaseBackend) SupportsRequestedLeaseID() bool { return true }

func (b *coordinatorLeaseBackend) RebindResolvedLeaseTarget(target *LeaseTarget, leaseID string) error {
	if rebinder, ok := b.direct.(ResolvedLeaseTargetRebinder); ok {
		return rebinder.RebindResolvedLeaseTarget(target, leaseID)
	}
	return nil
}

func (b *coordinatorLeaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	if strings.TrimSpace(req.RequestedLeaseID) != "" {
		acquired, err := b.acquireOnceWithLeaseID(ctx, req.Keep, strings.TrimSpace(req.RequestedLeaseID), req.RequestedSlug)
		if err != nil {
			return LeaseTarget{}, err
		}
		if req.OnAcquired != nil {
			if err := req.OnAcquired(acquired); err != nil {
				return LeaseTarget{}, fmt.Errorf("acknowledge fixed coordinator acquisition: %w", err)
			}
		}
		return acquired, nil
	}
	return acquireAttemptsRetry(b.rt, req.Keep, func() (LeaseTarget, error) {
		return b.acquireOnce(ctx, req.Keep, req.RequestedSlug)
	})
}

func (b *coordinatorLeaseBackend) acquireOnce(ctx context.Context, keep bool, requestedSlug string) (LeaseTarget, error) {
	return b.acquireOnceWithLeaseID(ctx, keep, "", requestedSlug)
}

func (b *coordinatorLeaseBackend) acquireOnceWithLeaseID(ctx context.Context, keep bool, requestedLeaseID, requestedSlug string) (LeaseTarget, error) {
	leaseID := requestedLeaseID
	if leaseID == "" {
		leaseID = newLeaseID()
	}
	var slug string
	var err error
	if requestedLeaseID != "" {
		slug = normalizeLeaseSlug(requestedSlug)
		if slug == "" {
			slug = newLeaseSlug(leaseID)
		}
	} else {
		slug, err = allocateClaimLeaseSlug(leaseID, requestedSlug)
		if err != nil {
			return LeaseTarget{}, err
		}
	}
	var keyPath, publicKey string
	keyAction := func() error {
		var keyErr error
		keyPath, publicKey, keyErr = ensureTestboxKeyForConfig(b.cfg, leaseID)
		return keyErr
	}
	if requestedLeaseID != "" {
		err = withLeaseIDOperationLock(leaseID, keyAction)
	} else {
		err = keyAction()
	}
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg := b.cfg
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
		cfg.Tailscale.Hostname = cfg.Tailscale.HostnameTemplate
	}
	cfg.AWSSSHCIDRsPinned = len(cfg.AWSSSHCIDRs) > 0
	ensureAWSSSHCIDRs(ctx, &cfg)
	fmt.Fprintf(b.rt.Stderr, "coordinator lease class=%s preferred_type=%s keep=%v slug=%s idle_timeout=%s ttl=%s\n", cfg.Class, cfg.ServerType, keep, slug, cfg.IdleTimeout, cfg.TTL)
	lease, err := b.createCoordinatorLeaseWithProgressMode(ctx, cfg, publicKey, keep, leaseID, slug, requestedLeaseID != "")
	if err != nil {
		if requestedLeaseID == "" {
			if isCoordinatorStaleInstanceCleanedSignal(err) {
				return LeaseTarget{}, coordinatorStaleInstanceCleanedError{err: err}
			}
			if isCoordinatorStaleInstanceError(err) && b.releaseStaleCoordinatorLeaseForRetry(leaseID) {
				return LeaseTarget{}, coordinatorStaleInstanceCleanedError{err: err}
			}
		}
		return LeaseTarget{}, err
	}
	if requestedLeaseID != "" && lease.ID != leaseID {
		return LeaseTarget{}, exit(4, "lease_id_conflict: coordinator fixed create returned lease %s for operation %s", blank(lease.ID, "<empty>"), leaseID)
	}
	if lease.ID != "" && lease.ID != leaseID {
		if err := moveStoredTestboxKey(leaseID, lease.ID); err != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: could not move local key from %s to %s: %v\n", leaseID, lease.ID, err)
		}
	}
	if err := validateCoordinatorLeaseCapabilities(cfg, lease); err != nil {
		if requestedLeaseID == "" {
			if releaseErr := releaseCoordinatorLease(context.Background(), b.coord, blank(lease.ID, leaseID)); releaseErr != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: release failed after capability mismatch for %s: %v\n", blank(lease.ID, leaseID), releaseErr)
			}
		}
		return LeaseTarget{}, err
	}
	server, target, leaseID := leaseToServerTarget(lease, cfg)
	fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s server=%d type=%s ip=%s via coordinator\n", leaseID, blank(lease.Slug, "-"), server.ID, server.ServerType.Name, target.Host)
	writeCoordinatorLeaseProvisioningDetails(b.rt.Stderr, lease)
	if summary := coordinatorFallbackSummary(lease); summary != "" {
		fmt.Fprintf(b.rt.Stderr, "fallback resolved %s\n", summary)
	}
	for _, line := range coordinatorCapacityHintLines(lease) {
		fmt.Fprintf(b.rt.Stderr, "capacity hint %s\n", line)
	}
	waitCtx, cancelWait := context.WithCancelCause(ctx)
	defer cancelWait(nil)
	stopHeartbeat := startCoordinatorHeartbeat(waitCtx, b.coord, leaseID, cfg.IdleTimeout, nil, leaseTelemetryCollectorForTarget(target), b.rt.Stderr)
	defer stopHeartbeat()
	stopLeaseWatch := startCoordinatorLeaseWatch(waitCtx, b.coord, leaseID, cancelWait, b.rt.Stderr)
	defer stopLeaseWatch()
	bootstrapTarget := bootstrapNetworkTarget(cfg, server, target)
	if err := bootstrapManagedWindowsDesktop(waitCtx, cfg, &bootstrapTarget, publicKey, b.rt.Stderr); err != nil {
		if requestedLeaseID == "" {
			if releaseErr := releaseCoordinatorLease(context.Background(), b.coord, leaseID); releaseErr != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: release failed after bootstrap error for %s: %v\n", leaseID, releaseErr)
			}
		}
		return LeaseTarget{}, err
	}
	target = bootstrapTarget
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: b.coord}, nil
}

func writeCoordinatorLeaseProvisioningDetails(stderr io.Writer, lease CoordinatorLease) {
	if lease.Image != nil {
		fmt.Fprintf(
			stderr,
			"image selected id=%s source=%s kind=%s region=%s promoted_at=%s\n",
			blank(lease.Image.ID, "-"),
			blank(lease.Image.Source, "-"),
			blank(lease.Image.Kind, "-"),
			blank(lease.Image.Region, "-"),
			blank(lease.Image.PromotedAt, "-"),
		)
	}
	if lease.ProvisioningTiming != nil {
		fmt.Fprintf(
			stderr,
			"provider startup request=%s network_ready=%s bootstrap=%s total=%s\n",
			formatMilliseconds(lease.ProvisioningTiming.RequestMs),
			formatMilliseconds(lease.ProvisioningTiming.NetworkReadyMs),
			formatMilliseconds(lease.ProvisioningTiming.BootstrapMs),
			formatMilliseconds(lease.ProvisioningTiming.TotalMs),
		)
	}
}

func formatMilliseconds(value int64) string {
	if value <= 0 {
		return "-"
	}
	return (time.Duration(value) * time.Millisecond).Round(time.Millisecond).String()
}

type coordinatorCreateLeaseResult struct {
	lease CoordinatorLease
	err   error
}

func (b *coordinatorLeaseBackend) createCoordinatorLeaseWithProgress(ctx context.Context, cfg Config, publicKey string, keep bool, leaseID, slug string) (CoordinatorLease, error) {
	return b.createCoordinatorLeaseWithProgressMode(ctx, cfg, publicKey, keep, leaseID, slug, false)
}

func (b *coordinatorLeaseBackend) createCoordinatorLeaseWithProgressMode(ctx context.Context, cfg Config, publicKey string, keep bool, leaseID, slug string, fixed bool) (CoordinatorLease, error) {
	timeout := coordinatorCreateLeaseTimeoutForConfig(cfg)
	createCtx, cancelCreate := context.WithTimeout(ctx, timeout)
	defer cancelCreate()
	resultCh := make(chan coordinatorCreateLeaseResult, 1)
	go func() {
		var lease CoordinatorLease
		var err error
		if fixed {
			lease, err = b.coord.EnsureLease(createCtx, cfg, publicKey, keep, leaseID, slug)
		} else {
			lease, err = b.coord.CreateLease(createCtx, cfg, publicKey, keep, leaseID, slug)
		}
		resultCh <- coordinatorCreateLeaseResult{lease: lease, err: err}
	}()

	ticker := time.NewTicker(coordinatorCreateLeaseProgressInterval)
	defer ticker.Stop()
	started := time.Now()
	for {
		select {
		case result := <-resultCh:
			if ctx.Err() != nil {
				return CoordinatorLease{}, b.canceledCoordinatorLeaseCreateError(ctx, leaseID, slug, fixed, result.err)
			}
			if fixed && result.err != nil {
				if lease, recoverErr, handled := b.recoverFixedCoordinatorLeaseAfterCreateError(ctx, cfg, publicKey, keep, leaseID, slug, result.err); handled {
					return lease, recoverErr
				}
				return CoordinatorLease{}, result.err
			}
			if result.err != nil && errors.Is(result.err, context.DeadlineExceeded) && ctx.Err() == nil {
				err := coordinatorCreateLeaseTimeoutError(cfg, leaseID, slug, timeout)
				if lease, ok := b.recoverCoordinatorLeaseAfterCreateError(ctx, cfg, leaseID, slug, err); ok {
					return lease, nil
				}
				return CoordinatorLease{}, err
			}
			if result.err != nil {
				if lease, ok := b.recoverCoordinatorLeaseAfterCreateError(ctx, cfg, leaseID, slug, result.err); ok {
					return lease, nil
				}
			}
			if result.err == nil && result.lease.State == "provisioning" {
				lease, err := b.waitForCoordinatorLeaseActivation(createCtx, leaseID, result.lease)
				if err != nil && ctx.Err() != nil {
					cancelErr := b.canceledCoordinatorLeaseCreateError(ctx, leaseID, slug, fixed, nil)
					return CoordinatorLease{}, errors.Join(cancelErr, definitiveCoordinatorCreateError(err))
				}
				return lease, err
			}
			return result.lease, result.err
		case <-ticker.C:
			fmt.Fprintf(b.rt.Stderr, "waiting for coordinator lease provider=%s slug=%s elapsed=%s timeout=%s\n", cfg.Provider, slug, time.Since(started).Round(time.Second), timeout)
		case <-createCtx.Done():
			if ctx.Err() != nil {
				return CoordinatorLease{}, b.canceledCoordinatorLeaseCreateError(ctx, leaseID, slug, fixed, nil)
			}
			err := coordinatorCreateLeaseTimeoutError(cfg, leaseID, slug, timeout)
			if fixed {
				if lease, recoverErr, handled := b.recoverFixedCoordinatorLeaseAfterCreateError(ctx, cfg, publicKey, keep, leaseID, slug, err); handled {
					return lease, recoverErr
				}
				return CoordinatorLease{}, err
			}
			if lease, ok := b.recoverCoordinatorLeaseAfterCreateError(ctx, cfg, leaseID, slug, err); ok {
				return lease, nil
			}
			return CoordinatorLease{}, err
		case <-ctx.Done():
			var createErr error
			select {
			case result := <-resultCh:
				createErr = result.err
			default:
			}
			return CoordinatorLease{}, b.canceledCoordinatorLeaseCreateError(ctx, leaseID, slug, fixed, createErr)
		}
	}
}

func (b *coordinatorLeaseBackend) canceledCoordinatorLeaseCreateError(ctx context.Context, leaseID, slug string, fixed bool, createErr error) error {
	callerErr := ctx.Err()
	if definitiveErr := definitiveCoordinatorCreateError(createErr); definitiveErr != nil {
		return errors.Join(callerErr, definitiveErr)
	}
	if fixed {
		return callerErr
	}
	recoverCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), coordinatorCanceledCreateRecoveryTimeout)
	defer cancel()
	fmt.Fprintf(b.rt.Stderr, "warning: coordinator lease create canceled for %s; reconciling late acceptance for cleanup\n", leaseID)
	if err := b.releaseCoordinatorLeaseAfterCanceledCreate(recoverCtx, leaseID, slug); err != nil {
		return errors.Join(callerErr, fmt.Errorf("reconcile canceled coordinator lease %s: %w", leaseID, err))
	}
	return callerErr
}

func (b *coordinatorLeaseBackend) releaseCoordinatorLeaseAfterCanceledCreate(ctx context.Context, leaseID, slug string) error {
	ticker := time.NewTicker(coordinatorCreateLeaseRecoveryInterval)
	defer ticker.Stop()
	for {
		result, err := b.coord.ReleaseLeaseAfterCanceledCreate(ctx, leaseID, true)
		if err == nil {
			return b.validateCanceledCreateRelease(result, leaseID, slug)
		}
		if !coordinatorCanceledCreateReleaseErrorRetryable(err) {
			return fmt.Errorf("request delete-release: %w", err)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return b.finalReleaseCoordinatorLeaseAfterCanceledCreate(ctx, leaseID, slug)
		}
	}
}

func (b *coordinatorLeaseBackend) finalReleaseCoordinatorLeaseAfterCanceledCreate(expiredCtx context.Context, leaseID, slug string) error {
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(expiredCtx), coordinatorCanceledCreateFinalTimeout)
	defer cancel()
	result, err := b.coord.ReleaseLeaseAfterCanceledCreate(finalCtx, leaseID, true)
	if err != nil {
		return errors.Join(expiredCtx.Err(), fmt.Errorf("final request delete-release: %w", err))
	}
	return b.validateCanceledCreateRelease(result, leaseID, slug)
}

func (b *coordinatorLeaseBackend) validateCanceledCreateRelease(result CoordinatorCanceledCreateReleaseResult, leaseID, slug string) error {
	if result.Lease.ID != leaseID && result.RequestLeaseID != leaseID {
		return fmt.Errorf("coordinator returned lease %q while releasing %q without matching requestLeaseID attestation", result.Lease.ID, leaseID)
	}
	fmt.Fprintf(b.rt.Stderr, "released coordinator lease %s slug=%s after canceled create\n", result.Lease.ID, blank(result.Lease.Slug, slug))
	return nil
}

func coordinatorCanceledCreateReleaseErrorRetryable(err error) bool {
	if isCoordinatorNotFoundError(err) || coordinatorCreateLeaseErrorMayHaveCommitted(err) {
		return true
	}
	var httpErr CoordinatorHTTPError
	return errors.As(err, &httpErr) &&
		(httpErr.StatusCode == http.StatusRequestTimeout ||
			httpErr.StatusCode == http.StatusTooManyRequests ||
			httpErr.StatusCode >= http.StatusInternalServerError)
}

func definitiveCoordinatorCreateError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || coordinatorCreateLeaseErrorMayHaveCommitted(err) {
		return nil
	}
	return fmt.Errorf("create coordinator lease: %w", err)
}

func (b *coordinatorLeaseBackend) recoverFixedCoordinatorLeaseAfterCreateError(ctx context.Context, cfg Config, publicKey string, keep bool, leaseID, slug string, createErr error) (CoordinatorLease, error, bool) {
	if !coordinatorCreateLeaseErrorMayHaveCommitted(createErr) {
		return CoordinatorLease{}, nil, false
	}
	recoverCtx, cancel := context.WithTimeout(ctx, coordinatorCreateLeaseRecoveryTimeout)
	defer cancel()
	fmt.Fprintf(b.rt.Stderr, "warning: coordinator fixed lease create returned uncertain result for %s; repeating exact PUT: %v\n", leaseID, createErr)
	ticker := time.NewTicker(coordinatorCreateLeaseRecoveryInterval)
	defer ticker.Stop()
	for {
		lease, err := b.coord.EnsureLease(recoverCtx, cfg, publicKey, keep, leaseID, slug)
		if err == nil {
			fmt.Fprintf(b.rt.Stderr, "confirmed coordinator fixed lease %s slug=%s with exact PUT after uncertain create\n", leaseID, blank(lease.Slug, slug))
			if lease.State == "provisioning" {
				active, waitErr := b.waitForCoordinatorLeaseActivation(recoverCtx, leaseID, lease)
				return active, waitErr, true
			}
			return lease, nil, true
		}
		if !coordinatorCreateLeaseErrorMayHaveCommitted(err) {
			return CoordinatorLease{}, err, true
		}
		select {
		case <-ticker.C:
		case <-recoverCtx.Done():
			return CoordinatorLease{}, errors.Join(createErr, recoverCtx.Err()), true
		}
	}
}

func (b *coordinatorLeaseBackend) waitForCoordinatorLeaseActivation(ctx context.Context, leaseID string, current CoordinatorLease) (CoordinatorLease, error) {
	ticker := time.NewTicker(coordinatorCreateLeaseRecoveryInterval)
	defer ticker.Stop()
	for {
		if current.State == "active" && current.Host != "" {
			return current, nil
		}
		if current.State == "failed" || current.State == "released" || current.State == "expired" {
			return CoordinatorLease{}, fmt.Errorf("coordinator fixed lease %s ended while provisioning: state=%s error=%s", leaseID, current.State, current.FailureError)
		}
		select {
		case <-ticker.C:
			lease, err := b.coord.GetLease(ctx, leaseID)
			if err != nil {
				if !coordinatorCreateLeaseErrorMayHaveCommitted(err) {
					return CoordinatorLease{}, err
				}
				continue
			}
			current = lease
		case <-ctx.Done():
			return CoordinatorLease{}, ctx.Err()
		}
	}
}

func (b *coordinatorLeaseBackend) recoverCoordinatorLeaseAfterCreateError(ctx context.Context, cfg Config, leaseID, slug string, createErr error) (CoordinatorLease, bool) {
	if !coordinatorCreateLeaseErrorMayHaveCommitted(createErr) {
		return CoordinatorLease{}, false
	}
	recoverCtx, cancel := context.WithTimeout(ctx, coordinatorCreateLeaseRecoveryTimeout)
	defer cancel()
	fmt.Fprintf(b.rt.Stderr, "warning: coordinator lease create returned uncertain result for %s; polling existing lease: %v\n", leaseID, createErr)
	ticker := time.NewTicker(coordinatorCreateLeaseRecoveryInterval)
	defer ticker.Stop()
	for {
		lease, err := b.coord.GetLease(recoverCtx, leaseID)
		if err == nil {
			if coordinatorLeaseRecoveredFromCreateError(cfg, lease) {
				fmt.Fprintf(b.rt.Stderr, "recovered coordinator lease %s slug=%s after uncertain create response\n", leaseID, blank(lease.Slug, slug))
				return lease, true
			}
			if lease.State == "failed" || lease.State == "released" || lease.State == "expired" {
				return CoordinatorLease{}, false
			}
		} else if !isCoordinatorNotFoundError(err) && !coordinatorCreateLeaseErrorMayHaveCommitted(err) {
			return CoordinatorLease{}, false
		}
		select {
		case <-ticker.C:
		case <-recoverCtx.Done():
			return CoordinatorLease{}, false
		}
	}
}

func coordinatorCreateLeaseErrorMayHaveCommitted(err error) bool {
	if err == nil {
		return false
	}
	if isCoordinatorStaleInstanceError(err) {
		return false
	}
	if isCoordinatorTransportError(err) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var httpErr CoordinatorHTTPError
	return errors.As(err, &httpErr) &&
		httpErr.StatusCode >= 500 &&
		strings.Contains(httpErr.Message, "error code: 1101")
}

func coordinatorLeaseRecoveredFromCreateError(cfg Config, lease CoordinatorLease) bool {
	if lease.ID == "" || lease.State != "active" || lease.Host == "" {
		return false
	}
	if lease.Provider != "" && !strings.EqualFold(lease.Provider, cfg.Provider) {
		return false
	}
	if lease.TargetOS != "" && cfg.TargetOS != "" && !strings.EqualFold(lease.TargetOS, cfg.TargetOS) {
		return false
	}
	return true
}

func defaultCoordinatorCreateLeaseTimeoutForConfig(cfg Config) time.Duration {
	if cfg.Provider == "azure" && cfg.TargetOS == targetLinux {
		return 10 * time.Minute
	}
	return coordinatorHTTPTimeout
}

func coordinatorCreateLeaseTimeoutError(cfg Config, leaseID, slug string, timeout time.Duration) error {
	serverType := blank(cfg.ServerType, "-")
	return fmt.Errorf(
		"timed out waiting for coordinator lease after %s provider=%s target=%s type=%s slug=%s lease=%s; no lease was returned; next_action=check coordinator/cloud logs and retry, then run `crabbox stop --provider %s --target %s --id %s` if a late lease appears",
		timeout,
		cfg.Provider,
		cfg.TargetOS,
		serverType,
		slug,
		leaseID,
		cfg.Provider,
		cfg.TargetOS,
		leaseID,
	)
}

func (b *coordinatorLeaseBackend) releaseStaleCoordinatorLeaseForRetry(leaseID string) bool {
	releaseCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := b.coord.ReleaseLease(releaseCtx, leaseID, true); err != nil {
		// A missing coordinator record means there is nothing left to discard.
		// Treat it as cleaned so acquire can retry with a new lease id.
		if isCoordinatorNotFoundError(err) {
			fmt.Fprintf(b.rt.Stderr, "stale coordinator lease %s was already gone; retrying with fresh lease\n", leaseID)
			return true
		}
		fmt.Fprintf(b.rt.Stderr, "warning: release failed after stale coordinator instance for %s; not retrying: %v\n", leaseID, err)
		return false
	}
	fmt.Fprintf(b.rt.Stderr, "discarded stale coordinator lease %s\n", leaseID)
	return true
}

func isCoordinatorStaleInstanceError(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "InvalidInstanceID.NotFound")
}

func isCoordinatorStaleInstanceCleanedSignal(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "crabbox_aws_stale_instance_cleaned") && isCoordinatorStaleInstanceError(err)
}

func (b *coordinatorLeaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	lease, err := b.coord.GetLease(ctx, req.ID)
	if err != nil {
		if b.cfg.CoordAdminToken != "" && (isCoordinatorNotFoundError(err) || isCoordinatorUnauthorized(err)) {
			adminCoord, adminErr := b.adminCoordinatorClient()
			if adminErr != nil {
				return LeaseTarget{}, err
			}
			lease, adminErr = adminCoord.GetLease(ctx, req.ID)
			if adminErr == nil {
				server, target, leaseID := leaseToServerTarget(lease, b.cfg)
				return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: adminCoord}, nil
			}
		}
		return LeaseTarget{}, err
	}
	server, target, leaseID := leaseToServerTarget(lease, b.cfg)
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: b.coord}, nil
}

func (b *coordinatorLeaseBackend) Status(ctx context.Context, req StatusRequest) (statusView, error) {
	var lease CoordinatorLease
	var err error
	if req.AuthoritativeProviderMetadata {
		lease, err = b.coord.GetLeaseWithAuthoritativeProviderMetadata(ctx, req.ID)
	} else {
		lease, err = b.coord.GetLease(ctx, req.ID)
	}
	if err != nil {
		return statusView{}, err
	}
	server, target, _ := leaseToServerTarget(lease, b.cfg)
	resolved, err := resolveNetworkTarget(ctx, b.cfg, server, target)
	if err != nil {
		return statusView{}, err
	}
	target = resolved.Target
	hasHost := lease.Host != ""
	ready := lease.State == "active" && hasHost && probeSSHReady(ctx, &target, 4*time.Second)
	return statusView{
		ID:               lease.ID,
		Slug:             lease.Slug,
		Provider:         blank(lease.Provider, b.cfg.Provider),
		TargetOS:         blank(target.TargetOS, b.cfg.TargetOS),
		WindowsMode:      blank(target.WindowsMode, b.cfg.WindowsMode),
		State:            lease.State,
		ServerID:         leaseDisplayID(lease),
		ServerType:       lease.ServerType,
		Host:             lease.Host,
		Network:          resolved.Network,
		Tailscale:        lease.Tailscale,
		SSHHost:          target.Host,
		SSHHostKey:       target.SSHHostKey,
		SSHUser:          redactedSSHUser(b.cfg, server, target),
		SSHPort:          target.Port,
		SSHFallbackPorts: target.FallbackPorts,
		SSHKey:           target.Key,
		LastTouchedAt:    lease.LastTouchedAt,
		IdleFor:          idleForString(lease.LastTouchedAt, time.Now()),
		IdleTimeout:      formatSecondsDuration(lease.IdleTimeoutSeconds),
		ExpiresAt:        lease.ExpiresAt,
		Labels:           map[string]string{"keep": fmt.Sprint(lease.Keep)},
		HasHost:          hasHost,
		Ready:            ready,
		Telemetry:        lease.Telemetry,
		TelemetryHistory: lease.TelemetryHistory,
		ProviderMetadata: inspectProviderMetadata(
			blank(lease.Provider, b.cfg.Provider),
			lease.ProviderMetadata,
		),
	}, nil
}

func (b *coordinatorLeaseBackend) List(ctx context.Context, req ListRequest) ([]Server, error) {
	if !req.All {
		leases, err := b.listUserLeases(ctx)
		if err != nil {
			return nil, err
		}
		return coordinatorLeasesToServers(leases, b.cfg), nil
	}
	machines, activeLeaseIDs, err := b.listMachines(ctx)
	if err != nil {
		leases, fallbackErr := b.listLeasesFallback(ctx, err)
		if fallbackErr != nil {
			return nil, fallbackErr
		}
		return coordinatorLeasesToServers(leases, b.cfg), nil
	}
	return coordinatorMachinesToServers(machines, activeLeaseIDs), nil
}

func (b *coordinatorLeaseBackend) ListJSON(ctx context.Context, req ListRequest) (any, error) {
	if !req.All {
		return b.listUserLeases(ctx)
	}
	machines, _, err := b.listMachines(ctx)
	if err != nil {
		return b.listLeasesFallback(ctx, err)
	}
	return machines, nil
}

func (b *coordinatorLeaseBackend) listUserLeases(ctx context.Context) ([]CoordinatorLease, error) {
	leases, err := b.coord.Leases(ctx, "active", 1000)
	if err != nil {
		return nil, err
	}
	return redactCoordinatorLeaseListSecrets(
		filterCoordinatorLeasesForProvider(leases, b.cfg.Provider),
	), nil
}

func (b *coordinatorLeaseBackend) listMachines(ctx context.Context) ([]CoordinatorMachine, map[string]struct{}, error) {
	if b.cfg.CoordAdminToken == "" {
		return nil, nil, exit(2, "pool list requires broker.adminToken or CRABBOX_COORDINATOR_ADMIN_TOKEN when a coordinator is configured")
	}
	cfg := b.cfg
	cfg.CoordToken = cfg.CoordAdminToken
	cfg.CoordTokenCommand = nil
	coord, _, err := newCoordinatorClient(cfg)
	if err != nil {
		return nil, nil, err
	}
	machines, err := coord.Pool(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	activeLeases, err := coord.AdminLeases(ctx, "active", "", "", 1000)
	if err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: active lease lookup failed; orphan status unavailable: %v\n", err)
		return machines, nil, nil
	}
	return machines, activeCoordinatorLeaseIDs(activeLeases), nil
}

func (b *coordinatorLeaseBackend) listLeasesFallback(ctx context.Context, adminErr error) ([]CoordinatorLease, error) {
	if b.cfg.CoordToken == "" && len(b.cfg.CoordTokenCommand) == 0 {
		return nil, adminErr
	}
	if adminErr != nil && isCoordinatorUnauthorized(adminErr) {
		fmt.Fprintf(b.rt.Stderr, "warning: coordinator admin pool list unauthorized; falling back to user-visible leases\n")
	} else if adminErr != nil && b.cfg.CoordAdminToken == "" {
		fmt.Fprintf(b.rt.Stderr, "warning: coordinator admin pool list unavailable; falling back to user-visible leases\n")
	} else if adminErr != nil {
		return nil, adminErr
	}
	return b.listUserLeases(ctx)
}

func coordinatorLeasesToServers(leases []CoordinatorLease, cfg Config) []Server {
	servers := make([]Server, 0, len(leases))
	for _, lease := range leases {
		server, _, _ := leaseToServerTarget(lease, cfg)
		servers = append(servers, server)
	}
	return servers
}

func filterCoordinatorLeasesForProvider(leases []CoordinatorLease, provider string) []CoordinatorLease {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return leases
	}
	out := make([]CoordinatorLease, 0, len(leases))
	for _, lease := range leases {
		if strings.EqualFold(strings.TrimSpace(lease.Provider), provider) {
			out = append(out, lease)
		}
	}
	return out
}

func redactCoordinatorLeaseListSecrets(leases []CoordinatorLease) []CoordinatorLease {
	out := append([]CoordinatorLease(nil), leases...)
	for i := range out {
		if strings.EqualFold(strings.TrimSpace(out[i].Provider), "daytona") && out[i].SSHUser != "" {
			out[i].SSHUser = "<token>"
		}
	}
	return out
}

func isCoordinatorUnauthorized(err error) bool {
	return err != nil && strings.Contains(err.Error(), "http 401")
}

func (b *coordinatorLeaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	if req.Lease.LeaseID == "" {
		return exit(2, "missing coordinator lease id")
	}
	if err := releaseCoordinatorLease(ctx, b.coord, req.Lease.LeaseID); err != nil {
		if b.cfg.CoordAdminToken != "" && (isCoordinatorNotFoundError(err) || isCoordinatorUnauthorized(err)) {
			adminCoord, adminErr := b.adminCoordinatorClient()
			if adminErr != nil {
				return err
			}
			if _, adminErr = adminCoord.AdminReleaseLease(ctx, req.Lease.LeaseID, true); adminErr == nil {
				removeLeaseClaim(req.Lease.LeaseID)
				return nil
			}
		}
		return err
	}
	removeLeaseClaim(req.Lease.LeaseID)
	return nil
}

func (b *coordinatorLeaseBackend) adminCoordinatorClient() (*CoordinatorClient, error) {
	cfg := b.cfg
	cfg.CoordToken = cfg.CoordAdminToken
	cfg.CoordTokenCommand = nil
	coord, _, err := newCoordinatorClient(cfg)
	return coord, err
}

func (b *coordinatorLeaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	lease, err := b.coord.TouchLease(ctx, req.Lease.LeaseID)
	if err != nil {
		return req.Lease.Server, err
	}
	server, _, _ := leaseToServerTarget(lease, b.cfg)
	return server, nil
}

func coordinatorMachinesToServers(machines []CoordinatorMachine, activeLeaseIDs map[string]struct{}) []Server {
	servers := make([]Server, 0, len(machines))
	for _, machine := range machines {
		labels := map[string]string{}
		for k, v := range machine.Labels {
			labels[k] = v
		}
		if activeLeaseIDs != nil {
			labels["orphan"] = strings.TrimSpace(coordinatorMachineOrphanField(labels, activeLeaseIDs))
		}
		server := Server{
			CloudID:  string(machine.ID),
			Provider: machine.Provider,
			Name:     machine.Name,
			Status:   machine.Status,
			Labels:   labels,
		}
		server.ServerType.Name = machine.ServerType
		server.PublicNet.IPv4.IP = machine.Host
		servers = append(servers, server)
	}
	return servers
}
