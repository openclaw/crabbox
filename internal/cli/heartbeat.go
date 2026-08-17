package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

type leaseHeartbeatView struct {
	ID            string `json:"id"`
	Slug          string `json:"slug,omitempty"`
	Provider      string `json:"provider"`
	State         string `json:"state"`
	LastTouchedAt string `json:"lastTouchedAt,omitempty"`
	IdleTimeout   string `json:"idleTimeout,omitempty"`
	ExpiresAt     string `json:"expiresAt,omitempty"`
}

func (a App) heartbeat(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("heartbeat", a.Stderr)
	provider := registerProviderSelectionFlag(fs, defaults, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	idleTimeout := fs.Duration("idle-timeout", 0, "replace the lease idle timeout while heartbeating")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	if fs.NArg() > 1 {
		return exit(2, "usage: crabbox heartbeat --id <lease-id-or-slug> [--provider <provider>] [--idle-timeout <duration>] [--json]")
	}
	idleTimeoutSet := flagWasSet(fs, "idle-timeout")
	if idleTimeoutSet && *idleTimeout <= 0 {
		return exit(2, "idle timeout must be positive")
	}

	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlagValues{}, networkModeFlagValues{}, leaseTargetConfigOptions{LeaseID: *id})
	if err != nil {
		return err
	}
	if err := requireLeaseID(*id, "crabbox heartbeat --id <lease-id-or-slug>", cfg); err != nil {
		return err
	}
	var idleTimeoutOverride *time.Duration
	if idleTimeoutSet {
		cfg.IdleTimeout = *idleTimeout
		idleTimeoutOverride = idleTimeout
	}

	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	if coordinator, ok := backend.(*coordinatorLeaseBackend); ok {
		lease, err := coordinator.HeartbeatLease(ctx, *id, idleTimeoutOverride)
		if err != nil {
			return err
		}
		return writeLeaseHeartbeatView(a.Stdout, heartbeatViewFromCoordinatorLease(lease), *jsonOut)
	}
	var registeredCoord *CoordinatorClient
	if shouldRegisterCoordinatorLease(cfg) {
		coord, configured, err := newCoordinatorClient(cfg)
		if err != nil {
			return err
		}
		if !configured {
			return exit(2, "provider=%s does not support lease heartbeat", backend.Spec().Name)
		}
		registeredCoord = coord
	}

	sshBackend, ok := backend.(SSHLeaseBackend)
	if !ok {
		return exit(2, "provider=%s does not support lease heartbeat", backend.Spec().Name)
	}
	lease, err := sshBackend.Resolve(ctx, ResolveRequest{
		Options:               leaseOptionsFromConfig(cfg),
		ID:                    *id,
		StatusOnly:            true,
		NoLocalStateMutations: true,
	})
	if err != nil {
		return err
	}
	state := blank(lease.Server.Labels["state"], lease.Server.Status)
	if statusTerminalState(state) {
		return exit(5, "lease %s is in terminal state %s", *id, state)
	}
	claimed, err := statusLeaseHasExactClaim(ctx, backend, lease, backend.Spec().Name, leaseOptionsFromConfig(cfg).ProviderScope)
	if err != nil {
		return err
	}
	if !claimed {
		return exit(4, "lease %s is not claimed for provider=%s; refusing heartbeat", *id, backend.Spec().Name)
	}

	if !idleTimeoutSet {
		if current, ok := directLeaseIdleTimeout(lease.Server.Labels); ok {
			cfg.IdleTimeout = current
			backend, err = loadBackend(cfg, runtimeForApp(a))
			if err != nil {
				return err
			}
			var supportsTouch bool
			sshBackend, supportsTouch = backend.(SSHLeaseBackend)
			if !supportsTouch {
				return exit(2, "provider=%s does not support lease heartbeat", backend.Spec().Name)
			}
		}
	}
	var registeredLease *CoordinatorLease
	if registeredCoord != nil {
		canonicalLeaseID := lease.LeaseID
		coordinatorLease, err := heartbeatCoordinatorLease(ctx, registeredCoord, canonicalLeaseID, cfg.Provider, idleTimeoutOverride)
		if err != nil {
			return err
		}
		if err := validateCoordinatorProviderIdentity(cfg.Provider, coordinatorLease.ID, coordinatorLease.Provider, true); err != nil {
			return err
		}
		if coordinatorLease.ID != canonicalLeaseID {
			return exit(4, "coordinator returned mismatched lease id: expected %s, found %s", canonicalLeaseID, blank(coordinatorLease.ID, "<empty>"))
		}
		registeredLease = &coordinatorLease
	}
	touched, err := sshBackend.Touch(ctx, TouchRequest{
		Lease:               lease,
		State:               state,
		IdleTimeout:         cfg.IdleTimeout,
		IdleTimeoutOverride: idleTimeoutOverride,
	})
	if err != nil {
		return err
	}
	if registeredLease != nil {
		return writeLeaseHeartbeatView(a.Stdout, heartbeatViewFromCoordinatorLease(*registeredLease), *jsonOut)
	}
	return writeLeaseHeartbeatView(a.Stdout, heartbeatViewFromServer(lease.LeaseID, touched), *jsonOut)
}

func directLeaseIdleTimeout(labels map[string]string) (time.Duration, bool) {
	if duration, ok := parseDurationSecondsLabel(labels["idle_timeout_secs"]); ok {
		return duration, true
	}
	return parseDurationSecondsLabel(labels["idle_timeout"])
}

func heartbeatViewFromCoordinatorLease(lease CoordinatorLease) leaseHeartbeatView {
	return leaseHeartbeatView{
		ID:            lease.ID,
		Slug:          lease.Slug,
		Provider:      lease.Provider,
		State:         lease.State,
		LastTouchedAt: lease.LastTouchedAt,
		IdleTimeout:   formatSecondsDuration(lease.IdleTimeoutSeconds),
		ExpiresAt:     lease.ExpiresAt,
	}
}

func heartbeatViewFromServer(leaseID string, server Server) leaseHeartbeatView {
	return leaseHeartbeatView{
		ID:            leaseID,
		Slug:          serverSlug(server),
		Provider:      server.Provider,
		State:         blank(server.Labels["state"], server.Status),
		LastTouchedAt: blank(leaseLabelTimeDisplay(server.Labels["last_touched_at"]), server.Labels["last_touched_at"]),
		IdleTimeout:   leaseLabelDurationDisplay(server.Labels["idle_timeout_secs"], server.Labels["idle_timeout"]),
		ExpiresAt:     blank(leaseLabelTimeDisplay(server.Labels["expires_at"]), server.Labels["expires_at"]),
	}
}

func writeLeaseHeartbeatView(stdout io.Writer, view leaseHeartbeatView, jsonOut bool) error {
	if jsonOut {
		return json.NewEncoder(stdout).Encode(view)
	}
	_, err := fmt.Fprintf(stdout, "heartbeat lease=%s slug=%s provider=%s state=%s idle_timeout=%s last_touched=%s expires=%s\n",
		view.ID,
		blank(view.Slug, "-"),
		view.Provider,
		view.State,
		blank(view.IdleTimeout, "-"),
		blank(view.LastTouchedAt, "-"),
		blank(view.ExpiresAt, "-"),
	)
	return err
}
