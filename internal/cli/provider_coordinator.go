package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	coordinatorCreateLeaseProgressInterval = 30 * time.Second
	coordinatorCreateLeaseTimeoutForConfig = defaultCoordinatorCreateLeaseTimeoutForConfig
)

type coordinatorLeaseBackend struct {
	spec   ProviderSpec
	cfg    Config
	direct SSHLeaseBackend
	coord  *CoordinatorClient
	rt     Runtime
}

func (b *coordinatorLeaseBackend) Spec() ProviderSpec { return b.spec }

func (b *coordinatorLeaseBackend) RebindResolvedLeaseTarget(target *LeaseTarget, leaseID string) error {
	if rebinder, ok := b.direct.(ResolvedLeaseTargetRebinder); ok {
		return rebinder.RebindResolvedLeaseTarget(target, leaseID)
	}
	return nil
}

func (b *coordinatorLeaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	return acquireAttemptsRetry(b.rt, req.Keep, func() (LeaseTarget, error) {
		return b.acquireOnce(ctx, req.Keep, req.RequestedSlug)
	})
}

func (b *coordinatorLeaseBackend) acquireOnce(ctx context.Context, keep bool, requestedSlug string) (LeaseTarget, error) {
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return LeaseTarget{}, err
	}
	keyPath, publicKey, err := ensureTestboxKeyForConfig(b.cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg := b.cfg
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
		cfg.Tailscale.Hostname = cfg.Tailscale.HostnameTemplate
	}
	ensureAWSSSHCIDRs(ctx, &cfg)
	fmt.Fprintf(b.rt.Stderr, "coordinator lease class=%s preferred_type=%s keep=%v slug=%s idle_timeout=%s ttl=%s\n", cfg.Class, cfg.ServerType, keep, slug, cfg.IdleTimeout, cfg.TTL)
	lease, err := b.createCoordinatorLeaseWithProgress(ctx, cfg, publicKey, keep, leaseID, slug)
	if err != nil {
		if isCoordinatorStaleInstanceCleanedSignal(err) {
			return LeaseTarget{}, coordinatorStaleInstanceCleanedError{err: err}
		}
		if isCoordinatorStaleInstanceError(err) && b.releaseStaleCoordinatorLeaseForRetry(leaseID) {
			return LeaseTarget{}, coordinatorStaleInstanceCleanedError{err: err}
		}
		return LeaseTarget{}, err
	}
	if lease.ID != "" && lease.ID != leaseID {
		if err := moveStoredTestboxKey(leaseID, lease.ID); err != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: could not move local key from %s to %s: %v\n", leaseID, lease.ID, err)
		}
	}
	if err := validateCoordinatorLeaseCapabilities(cfg, lease); err != nil {
		if releaseErr := releaseCoordinatorLease(context.Background(), b.coord, blank(lease.ID, leaseID)); releaseErr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: release failed after capability mismatch for %s: %v\n", blank(lease.ID, leaseID), releaseErr)
		}
		return LeaseTarget{}, err
	}
	server, target, leaseID := leaseToServerTarget(lease, cfg)
	fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s server=%d type=%s ip=%s via coordinator\n", leaseID, blank(lease.Slug, "-"), server.ID, server.ServerType.Name, target.Host)
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
		if releaseErr := releaseCoordinatorLease(context.Background(), b.coord, leaseID); releaseErr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: release failed after bootstrap error for %s: %v\n", leaseID, releaseErr)
		}
		return LeaseTarget{}, err
	}
	target = bootstrapTarget
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: b.coord}, nil
}

type coordinatorCreateLeaseResult struct {
	lease CoordinatorLease
	err   error
}

func (b *coordinatorLeaseBackend) createCoordinatorLeaseWithProgress(ctx context.Context, cfg Config, publicKey string, keep bool, leaseID, slug string) (CoordinatorLease, error) {
	timeout := coordinatorCreateLeaseTimeoutForConfig(cfg)
	createCtx, cancelCreate := context.WithTimeout(ctx, timeout)
	defer cancelCreate()
	resultCh := make(chan coordinatorCreateLeaseResult, 1)
	go func() {
		lease, err := b.coord.CreateLease(createCtx, cfg, publicKey, keep, leaseID, slug)
		resultCh <- coordinatorCreateLeaseResult{lease: lease, err: err}
	}()

	ticker := time.NewTicker(coordinatorCreateLeaseProgressInterval)
	defer ticker.Stop()
	started := time.Now()
	for {
		select {
		case result := <-resultCh:
			if result.err != nil && errors.Is(result.err, context.DeadlineExceeded) && ctx.Err() == nil {
				return CoordinatorLease{}, coordinatorCreateLeaseTimeoutError(cfg, leaseID, slug, timeout)
			}
			return result.lease, result.err
		case <-ticker.C:
			fmt.Fprintf(b.rt.Stderr, "waiting for coordinator lease provider=%s slug=%s elapsed=%s timeout=%s\n", cfg.Provider, slug, time.Since(started).Round(time.Second), timeout)
		case <-createCtx.Done():
			if ctx.Err() != nil {
				return CoordinatorLease{}, ctx.Err()
			}
			return CoordinatorLease{}, coordinatorCreateLeaseTimeoutError(cfg, leaseID, slug, timeout)
		case <-ctx.Done():
			return CoordinatorLease{}, ctx.Err()
		}
	}
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
	lease, err := b.coord.GetLease(ctx, req.ID)
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
		SSHUser:          target.User,
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
	}, nil
}

func (b *coordinatorLeaseBackend) List(ctx context.Context, req ListRequest) ([]Server, error) {
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
	_ = req
	machines, _, err := b.listMachines(ctx)
	if err != nil {
		return b.listLeasesFallback(ctx, err)
	}
	return machines, nil
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
	leases, err := b.coord.Leases(ctx, "active", 1000)
	if err != nil {
		return nil, err
	}
	return filterCoordinatorLeasesForProvider(leases, b.cfg.Provider), nil
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
