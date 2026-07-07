package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	} else if server.claimSnapshotSet {
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
	if err := a.registerCoordinatorLeaseBestEffort(ctx, cfg, LeaseTarget{
		Server:  server,
		SSH:     target,
		LeaseID: leaseID,
	}); err != nil {
		return claimed, err
	}
	return claimed, nil
}

func (a App) registerCoordinatorLeaseBestEffort(ctx context.Context, cfg Config, lease LeaseTarget) error {
	adapterID, workspaceID, adapterMode, bindingErr := adapterRuntimeRegistrationBinding()
	if bindingErr != nil {
		a.coordinatorRegistrationWarning(lease.LeaseID, bindingErr)
		return bindingErr
	}
	if !shouldRegisterCoordinatorLease(cfg) || strings.TrimSpace(lease.LeaseID) == "" {
		if adapterMode {
			err := fmt.Errorf("adapter workspace requires registered coordinator mode and a stable lease ID")
			a.coordinatorRegistrationWarning(lease.LeaseID, err)
			return err
		}
		return nil
	}
	coord, configured, err := newCoordinatorClient(cfg)
	if err != nil || !configured || coord == nil {
		if err == nil {
			err = fmt.Errorf("coordinator is not configured")
		}
		a.coordinatorRegistrationWarning(lease.LeaseID, err)
		if adapterMode {
			return err
		}
		return nil
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
		SSHUser:            coordinatorRegistrationSSHUser(target),
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
	if adapterMode {
		registrationID, err := ensureRuntimeAdapterRegistrationID(lease.LeaseID)
		if err != nil {
			a.coordinatorRegistrationWarning(lease.LeaseID, err)
			return err
		}
		registration.RuntimeAdapterID = adapterID
		registration.RuntimeWorkspaceID = workspaceID
		registration.RuntimeRegistrationID = registrationID
	}
	register := func() (CoordinatorLease, error) {
		callCtx, cancel := context.WithTimeout(ctx, coordinatorRegistrationTimeout)
		defer cancel()
		return coord.RegisterLease(callCtx, lease.LeaseID, registration)
	}
	registered, err := register()
	if adapterMode && runtimeAdapterRegistrationReplay(err) {
		registrationID, rotateErr := stageRuntimeAdapterRegistrationReplacement(
			lease.LeaseID,
			registration.RuntimeRegistrationID,
		)
		if rotateErr != nil {
			a.coordinatorRegistrationWarning(lease.LeaseID, rotateErr)
			return rotateErr
		}
		registration.RuntimeRegistrationID = registrationID
		registered, err = register()
	}
	if err != nil {
		a.coordinatorRegistrationWarning(lease.LeaseID, err)
		if adapterMode {
			return fmt.Errorf("register adapter workspace with coordinator: %w", err)
		}
		return nil
	}
	if adapterMode && (registered.RuntimeAdapterID != adapterID ||
		registered.RuntimeWorkspaceID != workspaceID ||
		registered.RuntimeRegistrationID != registration.RuntimeRegistrationID) {
		err := fmt.Errorf(
			"coordinator returned adapter binding %q/%q/%q, expected %q/%q/%q",
			registered.RuntimeAdapterID,
			registered.RuntimeWorkspaceID,
			registered.RuntimeRegistrationID,
			adapterID,
			workspaceID,
			registration.RuntimeRegistrationID,
		)
		a.coordinatorRegistrationWarning(lease.LeaseID, err)
		return err
	}
	if adapterMode {
		if err := acknowledgeRuntimeAdapterRegistrationID(
			lease.LeaseID,
			registration.RuntimeRegistrationID,
		); err != nil {
			a.coordinatorRegistrationWarning(lease.LeaseID, err)
			return err
		}
	}
	return nil
}

func coordinatorRegistrationSSHUser(target SSHTarget) string {
	if target.AuthSecret {
		return "<token>"
	}
	return target.User
}

func coordinatorRegistrationURLForConfig(cfg Config) (string, error) {
	if !shouldRegisterCoordinatorLease(cfg) {
		return "", nil
	}
	coord, configured, err := newCoordinatorClient(cfg)
	if err != nil {
		return "", err
	}
	if !configured || coord == nil || strings.TrimSpace(coord.BaseURL) == "" {
		return "", fmt.Errorf("registered coordinator mode has no configured coordinator")
	}
	return coord.BaseURL, nil
}

func validateControllerCoordinatorRegistrationURL(value string) error {
	if value == "" {
		return nil
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("coordinator registration URL must not contain surrounding whitespace")
	}
	normalized, err := coordinatorRegistrationURLForConfig(Config{
		BrokerMode:  BrokerModeRegistered,
		Coordinator: value,
	})
	if err != nil {
		return err
	}
	if normalized != value {
		return fmt.Errorf("coordinator registration URL must be canonical (%s)", normalized)
	}
	return nil
}

func adapterRuntimeRegistrationBinding() (adapterID, workspaceID string, required bool, err error) {
	adapterID = strings.TrimSpace(os.Getenv("CRABBOX_ADAPTER_ID"))
	workspaceID = strings.TrimSpace(os.Getenv(controllerWorkspaceIDEnv))
	required = adapterID != "" && workspaceID != ""
	if !required {
		return adapterID, workspaceID, false, nil
	}
	if !validControllerWorkspaceID(adapterID) || !validControllerWorkspaceID(workspaceID) {
		return adapterID, workspaceID, true, fmt.Errorf("adapter coordinator registration requires valid adapter and workspace IDs")
	}
	return adapterID, workspaceID, true, nil
}

func ensureRuntimeAdapterRegistrationID(leaseID string) (string, error) {
	var registrationID string
	err := mutateLeaseClaimGuarded(
		leaseID,
		func(claim leaseClaim, exists bool) error {
			if !exists || claim.LeaseID != leaseID {
				return fmt.Errorf("adapter coordinator registration requires a persisted lease claim")
			}
			return nil
		},
		func(claim *leaseClaim) error {
			current := strings.TrimSpace(claim.RuntimeAdapterRegistrationID)
			pending := strings.TrimSpace(claim.RuntimeAdapterPendingRegistrationID)
			if pending != "" {
				if !validControllerWorkspaceID(pending) {
					return fmt.Errorf("adapter coordinator registration has an invalid pending registration id")
				}
				registrationID = pending
				return nil
			}
			registrationID = current
			if current == "" {
				generated, err := randomHex(16)
				if err != nil {
					return fmt.Errorf("generate runtime adapter registration id: %w", err)
				}
				registrationID = generated
				claim.RuntimeAdapterRegistrationID = generated
			}
			if !validControllerWorkspaceID(registrationID) {
				return fmt.Errorf("adapter coordinator registration has an invalid registration id")
			}
			return nil
		},
	)
	return registrationID, err
}

func stageRuntimeAdapterRegistrationReplacement(leaseID, rejectedID string) (string, error) {
	var registrationID string
	err := mutateLeaseClaimGuarded(
		leaseID,
		func(claim leaseClaim, exists bool) error {
			if !exists || claim.LeaseID != leaseID {
				return fmt.Errorf("adapter coordinator registration requires a persisted lease claim")
			}
			return nil
		},
		func(claim *leaseClaim) error {
			current := strings.TrimSpace(claim.RuntimeAdapterRegistrationID)
			pending := strings.TrimSpace(claim.RuntimeAdapterPendingRegistrationID)
			if pending != "" && !validControllerWorkspaceID(pending) {
				return fmt.Errorf("adapter coordinator registration has an invalid pending registration id")
			}
			if pending != "" && pending != rejectedID {
				registrationID = pending
				return nil
			}
			if pending == rejectedID {
				claim.RuntimeAdapterRegistrationID = rejectedID
				claim.RuntimeAdapterPendingRegistrationID = ""
				current = rejectedID
			}
			if current != rejectedID {
				if !validControllerWorkspaceID(current) {
					return fmt.Errorf("adapter coordinator registration changed while rotating its generation")
				}
				registrationID = current
				return nil
			}
			generated, err := randomHex(16)
			if err != nil {
				return fmt.Errorf("rotate runtime adapter registration id: %w", err)
			}
			registrationID = generated
			claim.RuntimeAdapterPendingRegistrationID = generated
			return nil
		},
	)
	return registrationID, err
}

func acknowledgeRuntimeAdapterRegistrationID(leaseID, registrationID string) error {
	return mutateLeaseClaimGuarded(
		leaseID,
		func(claim leaseClaim, exists bool) error {
			if !exists || claim.LeaseID != leaseID {
				return fmt.Errorf("adapter coordinator registration requires a persisted lease claim")
			}
			return nil
		},
		func(claim *leaseClaim) error {
			current := strings.TrimSpace(claim.RuntimeAdapterRegistrationID)
			pending := strings.TrimSpace(claim.RuntimeAdapterPendingRegistrationID)
			switch {
			case pending == registrationID:
				claim.RuntimeAdapterRegistrationID = registrationID
				claim.RuntimeAdapterPendingRegistrationID = ""
			case current == registrationID:
				// Another registration attempt may already have staged the next
				// generation. Do not discard that independent pending transition.
			case registrationID == "":
				return fmt.Errorf("coordinator acknowledged an empty runtime adapter registration id")
			default:
				return fmt.Errorf("runtime adapter registration changed before acknowledgment")
			}
			return nil
		},
	)
}

func runtimeAdapterRegistrationReplay(err error) bool {
	return coordinatorResponseErrorCode(err, 409) == "runtime_adapter_registration_replayed"
}

func runtimeAdapterDeleteCompletionMismatch(err error) bool {
	return coordinatorResponseErrorCode(err, 409) == "runtime_adapter_delete_completion_mismatch"
}

func coordinatorResponseErrorCode(err error, status int) string {
	var httpErr CoordinatorHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != status {
		return ""
	}
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(httpErr.Message), &body) != nil {
		return ""
	}
	return body.Error
}

func (a App) coordinatorRegistrationWarning(leaseID string, err error) {
	if a.Stderr == nil {
		return
	}
	fmt.Fprintf(a.Stderr, "warning: coordinator registration failed for %s: %v\n", firstNonBlank(leaseID, "unknown"), err)
}

func (a App) startRegisteredWebVNCDaemonBestEffort(cfg Config, target SSHTarget, leaseID string, keep bool) {
	if !shouldStartRegisteredWebVNCDaemon(cfg, keep) {
		return
	}
	if err := a.startWebVNCDaemon(webVNCBridgeArgs(cfg, target, leaseID, false, false), leaseID, false, ""); err != nil {
		fmt.Fprintf(a.Stderr, "warning: could not start registered WebVNC bridge for %s: %v\n", leaseID, err)
	}
}

func shouldStartRegisteredWebVNCDaemon(cfg Config, keep bool) bool {
	// Controller warmup is a gated child lifecycle. Its desktop bridge is
	// created later with persisted ownership and no-provider-side-effects.
	// Never leave an ordinary registered-broker daemon outside that gate.
	return keep && cfg.Desktop && cfg.BrokerAutoWebVNC && shouldRegisterCoordinatorLease(cfg) &&
		strings.TrimSpace(os.Getenv(controllerWorkspaceIDEnv)) == ""
}

func (a App) releaseRegisteredCoordinatorLeaseBestEffort(ctx context.Context, cfg Config, leaseID string) {
	if strings.TrimSpace(os.Getenv(controllerWorkspaceIDEnv)) != "" {
		// The controller's stable-absence cleanup owns deregistration. Releasing
		// here would make a transient or eventually-consistent absence look final.
		return
	}
	if err := a.releaseRegisteredCoordinatorLease(ctx, cfg, leaseID, true); err != nil && a.Stderr != nil {
		fmt.Fprintf(a.Stderr, "warning: coordinator deregistration failed for %s: %v\n", leaseID, err)
	}
}

func (a App) releaseRegisteredCoordinatorLeaseAfterConfirmedAbsence(ctx context.Context, cfg Config, leaseID string) error {
	adapterID, workspaceID, adapterMode, err := adapterRuntimeRegistrationBinding()
	if err != nil {
		return err
	}
	if adapterMode {
		return a.completeRuntimeAdapterDeleteAfterConfirmedAbsence(ctx, cfg, leaseID, adapterID, workspaceID)
	}
	claim, exists, err := readLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if exists && (strings.TrimSpace(claim.RuntimeAdapterRegistrationID) != "" ||
		strings.TrimSpace(claim.RuntimeAdapterPendingRegistrationID) != "") {
		return fmt.Errorf("runtime adapter delete completion requires adapter binding for persisted registration id")
	}
	err = a.releaseRegisteredCoordinatorLease(ctx, cfg, leaseID, false)
	if isCoordinatorNotFound(err) {
		// Stable provider absence is already proven. A missing coordinator row is
		// the desired terminal state and makes this cleanup retry idempotent.
		return nil
	}
	return err
}

func (a App) completeRuntimeAdapterDeleteAfterConfirmedAbsence(ctx context.Context, cfg Config, leaseID, adapterID, workspaceID string) error {
	if !shouldRegisterCoordinatorLease(cfg) || strings.TrimSpace(leaseID) == "" {
		return nil
	}
	coord, configured, err := newCoordinatorClient(cfg)
	if err != nil || !configured || coord == nil {
		if err == nil {
			err = fmt.Errorf("coordinator is not configured")
		}
		return err
	}
	claim, exists, err := readLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	registrationIDs := uniqueNonBlankStrings(
		claim.RuntimeAdapterPendingRegistrationID,
		claim.RuntimeAdapterRegistrationID,
	)
	if !exists || claim.LeaseID != leaseID || len(registrationIDs) == 0 {
		return completeLegacyRuntimeAdapterDeleteAfterConfirmedAbsence(
			ctx,
			coord,
			leaseID,
			adapterID,
			workspaceID,
		)
	}
	for _, registrationID := range registrationIDs {
		if !validControllerWorkspaceID(registrationID) {
			return fmt.Errorf("runtime adapter delete completion has an invalid persisted registration id")
		}
		callCtx, cancel := context.WithTimeout(ctx, coordinatorRegistrationTimeout)
		_, err := coord.CompleteRuntimeAdapterDelete(callCtx, leaseID, adapterID, workspaceID, registrationID)
		cancel()
		if err == nil || isCoordinatorNotFound(err) {
			return nil
		}
		if runtimeAdapterDeleteCompletionMismatch(err) {
			continue
		}
		return err
	}
	return completeLegacyRuntimeAdapterDeleteAfterConfirmedAbsence(
		ctx,
		coord,
		leaseID,
		adapterID,
		workspaceID,
	)
}

func completeLegacyRuntimeAdapterDeleteAfterConfirmedAbsence(
	ctx context.Context,
	coord *CoordinatorClient,
	leaseID, adapterID, workspaceID string,
) error {
	callCtx, cancel := context.WithTimeout(ctx, coordinatorRegistrationTimeout)
	defer cancel()
	_, err := coord.CompleteLegacyRuntimeAdapterDelete(callCtx, leaseID, adapterID, workspaceID)
	if isCoordinatorNotFound(err) {
		return nil
	}
	return err
}

func (a App) releaseRegisteredCoordinatorLease(ctx context.Context, cfg Config, leaseID string, stopBridge bool) error {
	if !shouldRegisterCoordinatorLease(cfg) || strings.TrimSpace(leaseID) == "" {
		return nil
	}
	if stopBridge {
		if _, err := a.stopWebVNCDaemonIfRunning(leaseID); err != nil && a.Stderr != nil {
			fmt.Fprintf(a.Stderr, "warning: could not stop registered WebVNC bridge for %s: %v\n", leaseID, err)
		}
	}
	coord, configured, err := newCoordinatorClient(cfg)
	if err != nil || !configured || coord == nil {
		if err == nil {
			err = fmt.Errorf("coordinator is not configured")
		}
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, coordinatorRegistrationTimeout)
	defer cancel()
	if _, err := coord.ReleaseLease(callCtx, leaseID, false); err != nil {
		return err
	}
	return nil
}
