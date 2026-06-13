package cli

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const coordinatorRegistrationTimeout = 15 * time.Second

func (a App) claimLeaseTargetForRepoAndRegister(
	ctx context.Context,
	leaseID, slug string,
	cfg Config,
	server Server,
	target SSHTarget,
	repoRoot string,
	reclaim bool,
) error {
	_, err := a.claimLeaseTargetForRepoAndRegisterMode(ctx, leaseID, slug, cfg, server, target, repoRoot, reclaim, false)
	return err
}

func (a App) claimResolvedLeaseTargetForRepoAndRegister(
	ctx context.Context,
	leaseID, slug string,
	cfg Config,
	server Server,
	target SSHTarget,
	repoRoot string,
	reclaim bool,
) error {
	_, err := a.claimLeaseTargetForRepoAndRegisterMode(ctx, leaseID, slug, cfg, server, target, repoRoot, reclaim, true)
	return err
}

func (a App) claimLeaseTargetForRepoAndRegisterMode(
	ctx context.Context,
	leaseID, slug string,
	cfg Config,
	server Server,
	target SSHTarget,
	repoRoot string,
	reclaim, resolved bool,
) (leaseClaim, error) {
	var expected leaseClaim
	var expectedExists bool
	var err error
	if resolved {
		expected, expectedExists, err = resolvedLeaseClaimSnapshot(leaseID, server)
	} else {
		expected, expectedExists, err = readLeaseClaimWithPresence(leaseID)
	}
	if err != nil {
		return leaseClaim{}, err
	}
	claimed, err := claimLeaseTargetForRepoConfigIfUnchanged(
		leaseID,
		slug,
		cfg,
		server,
		target,
		repoRoot,
		cfg.IdleTimeout,
		reclaim,
		expected,
		expectedExists,
	)
	if err != nil {
		return leaseClaim{}, err
	}
	a.registerCoordinatorLeaseBestEffort(ctx, cfg, LeaseTarget{
		Server:  server,
		SSH:     target,
		LeaseID: leaseID,
	})
	return claimed, nil
}

func (a App) registerCoordinatorLeaseBestEffort(ctx context.Context, cfg Config, lease LeaseTarget) {
	if !shouldRegisterCoordinatorLease(cfg) || strings.TrimSpace(lease.LeaseID) == "" {
		return
	}
	coord, configured, err := newCoordinatorClient(cfg)
	if err != nil || !configured || coord == nil {
		fmt.Fprintf(a.Stderr, "warning: coordinator registration skipped for %s: %v\n", lease.LeaseID, err)
		return
	}
	server := lease.Server
	target := lease.SSH
	provider := firstNonBlank(server.Provider, cfg.Provider)
	targetOS := firstNonBlank(target.TargetOS, cfg.TargetOS)
	registration := CoordinatorLeaseRegistration{
		Slug:               firstNonBlank(serverSlug(server), lease.LeaseID),
		Provider:           provider,
		TargetOS:           targetOS,
		WindowsMode:        firstNonBlank(target.WindowsMode, cfg.WindowsMode),
		Desktop:            cfg.Desktop,
		DesktopEnv:         normalizedDesktopEnv(cfg.DesktopEnv),
		Browser:            cfg.Browser,
		Code:               cfg.Code,
		CloudID:            server.CloudID,
		ServerID:           server.ID,
		ServerName:         server.Name,
		ServerType:         firstNonBlank(server.ServerType.Name, cfg.ServerType),
		Host:               target.Host,
		SSHUser:            target.User,
		SSHPort:            target.Port,
		SSHFallbackPorts:   append([]string(nil), target.FallbackPorts...),
		WorkRoot:           cfg.WorkRoot,
		Profile:            cfg.Profile,
		Class:              cfg.Class,
		Pond:               normalizePondName(cfg.Pond),
		ExposedPorts:       append([]string(nil), cfg.ExposedPorts...),
		TTLSeconds:         int(cfg.TTL.Seconds()),
		IdleTimeoutSeconds: int(cfg.IdleTimeout.Seconds()),
	}
	callCtx, cancel := context.WithTimeout(ctx, coordinatorRegistrationTimeout)
	defer cancel()
	if _, err := coord.RegisterLease(callCtx, lease.LeaseID, registration); err != nil {
		fmt.Fprintf(a.Stderr, "warning: coordinator registration failed for %s: %v\n", lease.LeaseID, err)
	}
}

func (a App) startRegisteredWebVNCDaemonBestEffort(cfg Config, target SSHTarget, leaseID string, keep bool) {
	if !keep || !cfg.Desktop || !cfg.BrokerAutoWebVNC || !shouldRegisterCoordinatorLease(cfg) {
		return
	}
	if err := a.startWebVNCDaemon(webVNCBridgeArgs(cfg, target, leaseID, false, false), leaseID); err != nil {
		fmt.Fprintf(a.Stderr, "warning: could not start registered WebVNC bridge for %s: %v\n", leaseID, err)
	}
}

func (a App) releaseRegisteredCoordinatorLeaseBestEffort(ctx context.Context, cfg Config, leaseID string) {
	if !shouldRegisterCoordinatorLease(cfg) || strings.TrimSpace(leaseID) == "" {
		return
	}
	if _, err := a.stopWebVNCDaemonIfRunning(leaseID); err != nil {
		fmt.Fprintf(a.Stderr, "warning: could not stop registered WebVNC bridge for %s: %v\n", leaseID, err)
	}
	coord, configured, err := newCoordinatorClient(cfg)
	if err != nil || !configured || coord == nil {
		fmt.Fprintf(a.Stderr, "warning: coordinator deregistration skipped for %s: %v\n", leaseID, err)
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, coordinatorRegistrationTimeout)
	defer cancel()
	if _, err := coord.ReleaseLease(callCtx, leaseID, false); err != nil {
		fmt.Fprintf(a.Stderr, "warning: coordinator deregistration failed for %s: %v\n", leaseID, err)
	}
}
