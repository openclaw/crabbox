package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
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
	coordinatorCreateAttemptID               = newCreateAttemptID
)

type coordinatorLeaseBackend struct {
	spec   ProviderSpec
	cfg    Config
	direct SSHLeaseBackend
	coord  *CoordinatorClient
	rt     Runtime
}

type coordinatorProviderIdentityError struct {
	selectedProvider string
	returnedProvider string
	leaseID          string
}

func (e coordinatorProviderIdentityError) Error() string {
	return fmt.Sprintf(
		"coordinator lease provider identity mismatch: selected_provider=%s returned_provider=%s lease_id=%s",
		blank(e.selectedProvider, "<empty>"),
		blank(e.returnedProvider, "<empty>"),
		blank(e.leaseID, "<empty>"),
	)
}

func (e coordinatorProviderIdentityError) Unwrap() error {
	return exit(4, "%s", e.Error())
}

func isCoordinatorProviderIdentityError(err error) bool {
	var identityErr coordinatorProviderIdentityError
	return errors.As(err, &identityErr)
}

func canonicalProviderName(name string) (string, error) {
	provider, err := ProviderFor(strings.TrimSpace(name))
	if err != nil {
		return "", err
	}
	return provider.Name(), nil
}

func canonicalProvidersMatch(expected, actual string) bool {
	expected, err := canonicalProviderName(expected)
	if err != nil {
		return false
	}
	actual, err = canonicalProviderName(actual)
	return err == nil && actual == expected
}

func (b *coordinatorLeaseBackend) BeginRunFailureEvidence(ctx context.Context, req RunFailureEvidenceRequest) (RunFailureEvidenceCollector, error) {
	capability, ok := b.direct.(SSHRunFailureEvidenceBackend)
	if !ok {
		return nil, nil
	}
	return capability.BeginRunFailureEvidence(ctx, req)
}

func (b *coordinatorLeaseBackend) AcquireIsExclusiveOneShot() bool { return true }

func (b *coordinatorLeaseBackend) Spec() ProviderSpec { return b.spec }

func (b *coordinatorLeaseBackend) SupportsRequestedLeaseID() bool { return true }

func (b *coordinatorLeaseBackend) SupportsRequestedCheckpointID() bool { return true }

func (b *coordinatorLeaseBackend) ReleaseLeaseConnectionCleanupSafe() bool { return false }

func (b *coordinatorLeaseBackend) validateCoordinatorLeaseProviderIdentity(lease CoordinatorLease) error {
	return b.validateCoordinatorProviderIdentity(lease.ID, lease.Provider)
}

func (b *coordinatorLeaseBackend) validateCoordinatorProviderIdentity(leaseID, returnedProvider string) error {
	selectedProvider := strings.TrimSpace(b.spec.Name)
	if selectedProvider == "" {
		selectedProvider = strings.TrimSpace(b.cfg.Provider)
	}
	return validateCoordinatorProviderIdentity(selectedProvider, leaseID, returnedProvider, true)
}

func validateCoordinatorProviderIdentity(selectedProvider, leaseID, returnedProvider string, allowEmptyReturned bool) error {
	selectedProvider = strings.TrimSpace(selectedProvider)
	returnedProvider = strings.TrimSpace(returnedProvider)
	identityError := func() error {
		return coordinatorProviderIdentityError{
			selectedProvider: selectedProvider,
			returnedProvider: returnedProvider,
			leaseID:          strings.TrimSpace(leaseID),
		}
	}
	selectedProvider, err := canonicalProviderName(selectedProvider)
	if err != nil {
		return identityError()
	}
	if returnedProvider == "" && allowEmptyReturned {
		return nil
	}
	if canonicalProvidersMatch(selectedProvider, returnedProvider) {
		return nil
	}
	return identityError()
}

func (b *coordinatorLeaseBackend) expectedProvider() (string, error) {
	selectedProvider := strings.TrimSpace(b.spec.Name)
	if selectedProvider == "" {
		selectedProvider = strings.TrimSpace(b.cfg.Provider)
	}
	return canonicalProviderName(selectedProvider)
}

func (b *coordinatorLeaseBackend) coordinatorLeaseTargetForConfig(lease CoordinatorLease, cfg Config, coord *CoordinatorClient) (LeaseTarget, error) {
	if err := b.validateCoordinatorLeaseProviderIdentity(lease); err != nil {
		return LeaseTarget{}, err
	}
	lease, err := selectCoordinatorLeaseSSHPort(lease, cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	server, target, leaseID := leaseToServerTarget(lease, cfg)
	released := coordinatorProviderReleaseConfirmed(lease)
	if released {
		// Confirmed deletion retires guest access, not the release operation. Keep
		// platform metadata for local cleanup without recreating SSH trust.
		target = SSHTarget{TargetOS: target.TargetOS, WindowsMode: target.WindowsMode}
	} else if err := prepareLeaseSSHTrust(&target, leaseID); err != nil {
		return LeaseTarget{}, err
	}
	result := LeaseTarget{
		Server:       server,
		SSH:          target,
		LeaseID:      leaseID,
		Coordinator:  coord,
		runnerTiming: coordinatorRunnerTiming(lease),
	}
	if released {
		result.providerRelease = &leaseReleaseConfirmation{backend: b, leaseID: leaseID}
	}
	return result, nil
}

func selectCoordinatorLeaseSSHPort(lease CoordinatorLease, cfg Config) (CoordinatorLease, error) {
	if !IsSSHPortExplicit(&cfg) || lease.Host == "" || coordinatorProviderReleaseConfirmed(lease) {
		return lease, nil
	}
	// Explicit selection pins an advertised route before any SSH delivery;
	// it cannot add endpoints or replay a workload on another port.
	if !slices.Contains(append([]string{lease.SSHPort}, lease.SSHFallbackPorts...), cfg.SSHPort) {
		return CoordinatorLease{}, exit(2, "SSH port %s is not advertised by coordinator lease %s; select its primary or fallback port, or remove the --ssh-port, ssh.port, or CRABBOX_SSH_PORT override to use automatic selection", cfg.SSHPort, lease.ID)
	}
	lease.SSHPort, lease.SSHFallbackPorts = cfg.SSHPort, []string{}
	return lease, nil
}

func (b *coordinatorLeaseBackend) prepareCoordinatorLeaseAcquisition(lease CoordinatorLease, cfg Config) (LeaseTarget, SSHTarget, error) {
	resolved, err := b.coordinatorLeaseTargetForConfig(lease, cfg, b.coord)
	if err != nil {
		return LeaseTarget{}, SSHTarget{}, err
	}
	// Preserve the provider's initial routes separately from workload selection.
	// Do not fill in omitted advertised ports from local fallback defaults.
	authorizedPorts := append([]string{lease.SSHPort}, lease.SSHFallbackPorts...)
	initial := managedWindowsBootstrapTarget(cfg, resolved.SSH, authorizedPorts)
	return resolved, initial, nil
}

func (b *coordinatorLeaseBackend) RebindResolvedLeaseTarget(target *LeaseTarget, leaseID string) error {
	if rebinder, ok := b.direct.(ResolvedLeaseTargetRebinder); ok {
		return rebinder.RebindResolvedLeaseTarget(target, leaseID)
	}
	return nil
}

func (b *coordinatorLeaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	if validator, ok := b.direct.(CoordinatorAcquireValidator); ok {
		if err := validator.ValidateCoordinatorAcquire(); err != nil {
			return LeaseTarget{}, err
		}
	}
	claim, checkpointBacked := checkpointLeaseClaimFromContext(ctx)
	if req.RequestedLeaseID != "" && ((req.RequestedCheckpointID != "") != checkpointBacked || checkpointBacked && req.RequestedCheckpointID != claim.CheckpointID) {
		return LeaseTarget{}, exit(2, "fixed coordinator checkpoint acquisition requires its exact checkpoint use context")
	}
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
			if isCoordinatorStaleInstanceCleanedError(err) {
				return LeaseTarget{}, err
			}
			if isCoordinatorStaleInstanceCleanedSignal(err) {
				return LeaseTarget{}, coordinatorStaleInstanceCleanedError{err: err}
			}
		}
		return LeaseTarget{}, err
	}
	if lease.ID != "" && lease.ID != leaseID {
		if err := moveStoredTestboxKey(leaseID, lease.ID); err != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: could not move local key from %s to %s: %v\n", leaseID, lease.ID, err)
		}
	}
	if err := validateCoordinatorLeaseCapabilities(cfg, lease); err != nil {
		if requestedLeaseID == "" {
			cleanupLeaseID := blank(lease.ID, leaseID)
			released, releaseErr := releaseCoordinatorLeaseResult(context.Background(), b.coord, cleanupLeaseID, cfg.Provider)
			reportCoordinatorAcquisitionRollback(b.rt.Stderr, cleanupLeaseID, "capability mismatch", released, releaseErr)
		}
		return LeaseTarget{}, err
	}
	resolvedLease, initialTarget, err := b.prepareCoordinatorLeaseAcquisition(lease, cfg)
	if err != nil {
		if requestedLeaseID == "" {
			cleanupLeaseID := blank(lease.ID, leaseID)
			released, releaseErr := releaseCoordinatorLeaseResult(context.Background(), b.coord, cleanupLeaseID, cfg.Provider)
			reportCoordinatorAcquisitionRollback(b.rt.Stderr, cleanupLeaseID, "SSH trust preparation error", released, releaseErr)
		}
		return LeaseTarget{}, err
	}
	server, target, leaseID := resolvedLease.Server, resolvedLease.SSH, resolvedLease.LeaseID
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
	stopHeartbeat, err := startCoordinatorHeartbeat(waitCtx, b.coord, leaseID, cfg.Provider, cfg.IdleTimeout, nil, leaseTelemetryCollectorForTarget(target), b.rt.Stderr)
	if err != nil {
		return LeaseTarget{}, err
	}
	defer stopHeartbeat()
	stopLeaseWatch := startCoordinatorLeaseWatch(waitCtx, b.coord, leaseID, cancelWait, b.rt.Stderr)
	defer stopLeaseWatch()
	target = bootstrapNetworkTarget(cfg, server, target)
	initialTarget = bootstrapNetworkTarget(cfg, server, initialTarget)
	if err := bootstrapPreparedManagedWindowsDesktop(waitCtx, cfg, &target, initialTarget, publicKey, b.rt.Stderr); err != nil {
		if requestedLeaseID == "" {
			released, releaseErr := releaseCoordinatorLeaseResult(context.Background(), b.coord, leaseID, cfg.Provider)
			reportCoordinatorAcquisitionRollback(b.rt.Stderr, leaseID, "bootstrap error", released, releaseErr)
		}
		return LeaseTarget{}, err
	}
	return LeaseTarget{
		Server:       server,
		SSH:          target,
		LeaseID:      leaseID,
		Coordinator:  b.coord,
		runnerTiming: coordinatorRunnerTiming(lease),
	}, nil
}

func reportCoordinatorAcquisitionRollback(stderr io.Writer, leaseID, reason string, released CoordinatorLease, releaseErr error) {
	if releaseErr != nil {
		fmt.Fprintf(stderr, "warning: release failed after %s for %s: %v\n", reason, leaseID, releaseErr)
		return
	}
	if coordinatorProviderReleaseConfirmed(released) {
		cleanupReleasedCoordinatorLeaseArtifacts(context.Background(), stderr, leaseID)
		return
	}
	state := "returned an unexpected non-final state"
	switch {
	case retainedCoordinatorRelease(released):
		state = "retained the provider resource"
	case coordinatorReleaseCleanupFailed(released):
		state = "reported a cleanup failure or scheduled retry"
	case coordinatorReleaseCleanupPending(released):
		state = "is still pending"
	}
	fmt.Fprintf(stderr, "warning: release after %s for %s did not confirm provider cleanup (%s); local SSH artifacts were preserved\n", reason, leaseID, state)
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

func coordinatorRunnerTiming(lease CoordinatorLease) *runnerProviderTiming {
	timing := lease.ProvisioningTiming
	if timing == nil || timing.TotalMs <= 0 {
		return nil
	}
	if phases, ok := coordinatorRunnerPhases(timing.Phases, timing.TotalMs); ok {
		return &runnerProviderTiming{TotalMs: timing.TotalMs, Phases: phases}
	}
	phases, ok := coordinatorLegacyRunnerPhases(timing)
	if !ok {
		phases = []RunnerPhase{{Name: "provider.unattributed", Ms: timing.TotalMs}}
	}
	return &runnerProviderTiming{TotalMs: timing.TotalMs, Phases: phases}
}

func coordinatorRunnerPhases(phases []CoordinatorProvisioningPhase, totalMs int64) ([]RunnerPhase, bool) {
	if len(phases) == 0 || len(phases) > 4 || totalMs <= 0 {
		return nil, false
	}
	seen := make(map[string]struct{}, len(phases))
	result := make([]RunnerPhase, 0, len(phases)+1)
	remaining := totalMs
	unattributedIndex := -1
	for _, phase := range phases {
		name := strings.TrimSpace(phase.Name)
		if phase.Ms <= 0 || phase.Ms > remaining {
			return nil, false
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, false
		}
		seen[name] = struct{}{}
		runnerName := ""
		switch name {
		case "request":
			runnerName = "provider.request"
		case "network_ready":
			runnerName = "connect.provider"
		case "bootstrap":
			runnerName = "bootstrap.readiness"
		case "unattributed":
			runnerName = "provider.unattributed"
			unattributedIndex = len(result)
		default:
			return nil, false
		}
		result = append(result, RunnerPhase{Name: runnerName, Ms: phase.Ms})
		remaining -= phase.Ms
	}
	if remaining > 0 {
		if unattributedIndex >= 0 {
			result[unattributedIndex].Ms += remaining
		} else {
			result = append(result, RunnerPhase{Name: "provider.unattributed", Ms: remaining})
		}
	}
	return result, true
}

func coordinatorLegacyRunnerPhases(timing *CoordinatorProvisioningTiming) ([]RunnerPhase, bool) {
	if timing == nil || timing.TotalMs <= 0 ||
		timing.RequestMs < 0 || timing.NetworkReadyMs < 0 || timing.BootstrapMs < 0 {
		return nil, false
	}
	remaining := timing.TotalMs
	result := make([]RunnerPhase, 0, 4)
	appendPhase := func(name string, ms int64) bool {
		if ms == 0 {
			return true
		}
		if ms > remaining {
			return false
		}
		result = append(result, RunnerPhase{Name: name, Ms: ms})
		remaining -= ms
		return true
	}
	if !appendPhase("provider.request", timing.RequestMs) ||
		!appendPhase("connect.provider", timing.NetworkReadyMs) ||
		!appendPhase("bootstrap.readiness", timing.BootstrapMs) {
		return nil, false
	}
	if remaining > 0 {
		result = append(result, RunnerPhase{Name: "provider.unattributed", Ms: remaining})
	}
	return result, true
}

func (b *coordinatorLeaseBackend) createCoordinatorLeaseWithProgress(ctx context.Context, cfg Config, publicKey string, keep bool, leaseID, slug string) (CoordinatorLease, error) {
	return b.createCoordinatorLeaseWithProgressMode(ctx, cfg, publicKey, keep, leaseID, slug, false)
}

func (b *coordinatorLeaseBackend) createCoordinatorLeaseWithProgressMode(ctx context.Context, cfg Config, publicKey string, keep bool, leaseID, slug string, fixed bool) (lease CoordinatorLease, err error) {
	started := time.Now()
	timeout := coordinatorCreateLeaseTimeoutForConfig(cfg)
	createCtx, cancelCreate := context.WithTimeout(ctx, timeout)
	defer cancelCreate()

	createAttemptID := ""
	if !fixed {
		createAttemptID = coordinatorCreateAttemptID()
	}
	// Progress must continue through replay and activation, including stalled GETs.
	// Keep writes serialized and join the reporter before returning to the caller.
	progressBackend := *b
	progressBackend.rt.Stderr = &synchronizedFanoutWriter{writers: []io.Writer{b.rt.Stderr}}
	b = &progressBackend
	progressCtx, stopProgress := context.WithCancel(createCtx)
	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		ticker := time.NewTicker(coordinatorCreateLeaseProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				fmt.Fprintf(b.rt.Stderr, "waiting for coordinator lease provider=%s slug=%s elapsed=%s timeout=%s\n", cfg.Provider, slug, time.Since(started).Round(time.Second), timeout)
			case <-progressCtx.Done():
				return
			}
		}
	}()
	cancelOnError := false
	defer func() {
		stopProgress()
		<-progressDone
		if ctx.Err() != nil {
			lease = CoordinatorLease{}
			err = b.canceledCoordinatorLeaseCreateError(ctx, leaseID, slug, createAttemptID, fixed, err)
			return
		}
		if createCtx.Err() != nil {
			lease = CoordinatorLease{}
			err = errors.Join(coordinatorCreateLeaseTimeoutError(cfg, leaseID, slug, timeout), definitiveCoordinatorCreateError(err))
			cancelOnError = true
		}
		if err != nil {
			lease = CoordinatorLease{}
			if !fixed && cancelOnError {
				err = b.abandonUnrecoveredCoordinatorLeaseCreate(ctx, leaseID, slug, createAttemptID, err)
				err = errors.Join(ctx.Err(), err)
			}
		}
	}()

	create := func(requestCtx context.Context) (CoordinatorLease, error) {
		if fixed {
			return b.coord.EnsureLease(requestCtx, cfg, publicKey, keep, leaseID, slug)
		}
		return b.coord.CreateLeaseWithAttempt(requestCtx, cfg, publicKey, keep, leaseID, slug, createAttemptID)
	}
	rebound := false
	lease, err = create(createCtx)
	if err != nil {
		cancelOnError = coordinatorCreateLeaseErrorMayHaveCommitted(err) ||
			(isCoordinatorStaleInstanceError(err) && !isCoordinatorStaleInstanceCleanedSignal(err))
		if coordinatorCreateLeaseErrorMayHaveCommitted(err) && createCtx.Err() == nil {
			lease, err = b.recoverCoordinatorLeaseAfterCreateError(createCtx, leaseID, fixed, create, err)
			rebound = err == nil
		}
		if err != nil {
			return CoordinatorLease{}, err
		}
	}
	if err := createCtx.Err(); err != nil {
		return CoordinatorLease{}, err
	}
	cancelOnError = true
	if err := b.validateCoordinatorLeaseCreateResult(cfg, lease, leaseID, fixed); err != nil {
		return CoordinatorLease{}, err
	}
	if lease.State == "provisioning" {
		// Rebinding only confirms the operation. Readiness uses the original deadline.
		return b.waitForCoordinatorLeaseActivation(createCtx, cfg, lease.ID, lease)
	}
	if rebound && lease.State == "active" && lease.Host == "" {
		return CoordinatorLease{}, fmt.Errorf("coordinator replay returned active lease %s without an endpoint", lease.ID)
	}
	return lease, coordinatorLeaseProvisioningError(lease)
}

func (b *coordinatorLeaseBackend) validateCoordinatorLeaseCreateResult(cfg Config, lease CoordinatorLease, requestedLeaseID string, fixed bool) error {
	if fixed && lease.ID != requestedLeaseID {
		return exit(4, "lease_id_conflict: coordinator returned lease %s for operation %s", blank(lease.ID, "<empty>"), requestedLeaseID)
	}
	if strings.TrimSpace(lease.ID) == "" {
		return exit(4, "coordinator create returned an empty canonical lease id for operation %s", requestedLeaseID)
	}
	if err := b.validateCoordinatorLeaseProviderIdentity(lease); err != nil {
		return err
	}
	if lease.TargetOS != "" && cfg.TargetOS != "" && !strings.EqualFold(lease.TargetOS, cfg.TargetOS) {
		return exit(4, "coordinator lease target mismatch: requested=%s returned=%s lease=%s", cfg.TargetOS, lease.TargetOS, lease.ID)
	}
	if cfg.TargetOS == targetWindows && cfg.WindowsMode != "" && lease.WindowsMode != "" && lease.WindowsMode != cfg.WindowsMode {
		return exit(4, "coordinator lease Windows mode mismatch: requested=%s returned=%s lease=%s", cfg.WindowsMode, lease.WindowsMode, lease.ID)
	}
	return nil
}

func (b *coordinatorLeaseBackend) canceledCoordinatorLeaseCreateError(ctx context.Context, leaseID, slug, createAttemptID string, fixed bool, createErr error) error {
	callerErr := ctx.Err()
	definitiveErr := definitiveCoordinatorCreateError(createErr)
	if fixed {
		return errors.Join(callerErr, definitiveErr)
	}
	recoverCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), coordinatorCanceledCreateRecoveryTimeout)
	defer cancel()
	fmt.Fprintf(b.rt.Stderr, "warning: coordinator lease create canceled for %s; recording durable create cancellation\n", leaseID)
	if err := b.cancelCoordinatorLeaseCreate(recoverCtx, leaseID, slug, createAttemptID); err != nil {
		return errors.Join(callerErr, definitiveErr, fmt.Errorf("cancel coordinator lease create %s: %w", leaseID, err))
	}
	return errors.Join(callerErr, definitiveErr)
}

func (b *coordinatorLeaseBackend) abandonUnrecoveredCoordinatorLeaseCreate(ctx context.Context, leaseID, slug, createAttemptID string, createErr error) error {
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), coordinatorCanceledCreateRecoveryTimeout)
	defer cancel()
	fmt.Fprintf(b.rt.Stderr, "warning: abandoning uncertain coordinator create %s; recording durable cancellation\n", leaseID)
	if err := b.cancelCoordinatorLeaseCreate(cancelCtx, leaseID, slug, createAttemptID); err != nil {
		return errors.Join(createErr, fmt.Errorf("cancel unrecovered coordinator lease create %s: %w", leaseID, err))
	}
	if isCoordinatorStaleInstanceError(createErr) {
		return coordinatorStaleInstanceCleanedError{err: createErr}
	}
	return createErr
}

func (b *coordinatorLeaseBackend) cancelCoordinatorLeaseCreate(ctx context.Context, leaseID, slug, createAttemptID string) error {
	ticker := time.NewTicker(coordinatorCreateLeaseRecoveryInterval)
	defer ticker.Stop()
	for {
		result, err := b.coord.CancelLeaseCreate(ctx, leaseID, createAttemptID)
		if err == nil {
			return b.validateCanceledCreateAttestation(result, leaseID, slug, createAttemptID)
		}
		if !coordinatorCancelCreateErrorRetryable(err) {
			return fmt.Errorf("request cancel-create: %w", err)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return b.finalCancelCoordinatorLeaseCreate(ctx, leaseID, slug, createAttemptID)
		}
	}
}

func (b *coordinatorLeaseBackend) finalCancelCoordinatorLeaseCreate(expiredCtx context.Context, leaseID, slug, createAttemptID string) error {
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(expiredCtx), coordinatorCanceledCreateFinalTimeout)
	defer cancel()
	result, err := b.coord.CancelLeaseCreate(finalCtx, leaseID, createAttemptID)
	if err != nil {
		return errors.Join(expiredCtx.Err(), fmt.Errorf("final request cancel-create: %w", err))
	}
	return b.validateCanceledCreateAttestation(result, leaseID, slug, createAttemptID)
}

func (b *coordinatorLeaseBackend) validateCanceledCreateAttestation(result CoordinatorCanceledCreateResult, leaseID, slug, createAttemptID string) error {
	attestation := result.CanceledCreate
	if attestation.Version != 1 || attestation.RequestedLeaseID != leaseID || attestation.CreateAttemptID != createAttemptID || attestation.State != "canceled" {
		return fmt.Errorf("coordinator returned mismatched cancel-create attestation for lease %q attempt %q", leaseID, createAttemptID)
	}
	fmt.Fprintf(b.rt.Stderr, "recorded canceled coordinator create %s slug=%s\n", leaseID, slug)
	return nil
}

func coordinatorCancelCreateErrorRetryable(err error) bool {
	if isCoordinatorTransportError(err) || errors.Is(err, context.DeadlineExceeded) {
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

func (b *coordinatorLeaseBackend) recoverCoordinatorLeaseAfterCreateError(
	ctx context.Context,
	leaseID string,
	fixed bool,
	create func(context.Context) (CoordinatorLease, error),
	createErr error,
) (CoordinatorLease, error) {
	recoverCtx, cancel := context.WithTimeout(ctx, coordinatorCreateLeaseRecoveryTimeout)
	defer cancel()
	replay := "token-bound POST"
	if fixed {
		replay = "exact PUT"
	}
	fmt.Fprintf(b.rt.Stderr, "warning: coordinator lease create returned uncertain result for %s; repeating %s: %v\n", leaseID, replay, createErr)
	ticker := time.NewTicker(coordinatorCreateLeaseRecoveryInterval)
	defer ticker.Stop()
	for {
		lease, err := create(recoverCtx)
		if recoverCtx.Err() != nil {
			return CoordinatorLease{}, errors.Join(createErr, recoverCtx.Err(), definitiveCoordinatorCreateError(err))
		}
		if err == nil {
			return lease, nil
		}
		if !coordinatorCreateLeaseErrorMayHaveCommitted(err) {
			return CoordinatorLease{}, err
		}
		select {
		case <-ticker.C:
		case <-recoverCtx.Done():
			return CoordinatorLease{}, errors.Join(createErr, recoverCtx.Err())
		}
	}
}

func (b *coordinatorLeaseBackend) waitForCoordinatorLeaseActivation(ctx context.Context, cfg Config, leaseID string, current CoordinatorLease) (CoordinatorLease, error) {
	ticker := time.NewTicker(coordinatorCreateLeaseRecoveryInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return CoordinatorLease{}, err
		}
		if current.State == "active" && current.Host != "" {
			return current, nil
		}
		if err := coordinatorLeaseProvisioningError(current); err != nil {
			return CoordinatorLease{}, err
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
			// GET observes the confirmed canonical lease; it cannot remap identity.
			if err := b.validateCoordinatorLeaseCreateResult(cfg, lease, leaseID, true); err != nil {
				return CoordinatorLease{}, err
			}
			current = lease
		case <-ctx.Done():
			return CoordinatorLease{}, ctx.Err()
		}
	}
}

type coordinatorLeaseProvisioningStateError struct {
	message string
}

func (e coordinatorLeaseProvisioningStateError) Error() string { return e.message }

func coordinatorLeaseProvisioningError(lease CoordinatorLease) error {
	switch lease.State {
	case "active", "provisioning":
		return nil
	case "failed", "released", "expired":
		return coordinatorLeaseProvisioningStateError{message: fmt.Sprintf("coordinator lease %s ended while provisioning: state=%s error=%s", lease.ID, lease.State, lease.FailureError)}
	default:
		return coordinatorLeaseProvisioningStateError{message: fmt.Sprintf("coordinator lease %s returned unexpected provisioning state %q", lease.ID, lease.State)}
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
	return errors.As(err, &httpErr) && httpErr.StatusCode >= http.StatusInternalServerError
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
		"timed out waiting for coordinator lease after %s provider=%s target=%s type=%s slug=%s lease=%s; no lease was returned; next_action=check coordinator/cloud logs and retry, then run `crabbox stop --provider %s --target %s --id %s` if a late lease appears: %w",
		timeout,
		cfg.Provider,
		cfg.TargetOS,
		serverType,
		slug,
		leaseID,
		cfg.Provider,
		cfg.TargetOS,
		leaseID,
		context.DeadlineExceeded,
	)
}

func isCoordinatorStaleInstanceError(err error) bool {
	if err == nil {
		return false
	}
	// Terminal canonical operations cannot authorize a fresh allocation, even if
	// their stored provider diagnostic contains an old stale-instance signal.
	var stateErr coordinatorLeaseProvisioningStateError
	if errors.As(err, &stateErr) {
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

const coordinatorReleaseResolveTimeout = 10 * time.Second

func (b *coordinatorLeaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	cfg := b.cfg
	if req.ReleaseOnly {
		// Provider cleanup must not depend on an optional guest route selection.
		cfg.explicitSSHPort = ""
		// Leave the caller's budget available for the provider-scoped release fallback.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, coordinatorReleaseResolveTimeout)
		defer cancel()
	}
	lease, err := b.coord.GetLease(ctx, req.ID)
	if err != nil {
		if b.cfg.CoordAdminToken != "" && (isCoordinatorNotFoundError(err) || isCoordinatorUnauthorized(err)) {
			adminCoord, adminErr := b.adminCoordinatorClient()
			if adminErr != nil {
				return LeaseTarget{}, err
			}
			lease, adminErr = adminCoord.GetLease(ctx, req.ID)
			if adminErr == nil {
				return b.coordinatorLeaseTargetForConfig(lease, cfg, adminCoord)
			}
		}
		return LeaseTarget{}, err
	}
	return b.coordinatorLeaseTargetForConfig(lease, cfg, b.coord)
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
	if err := b.validateCoordinatorLeaseProviderIdentity(lease); err != nil {
		return statusView{}, err
	}
	lease, err = selectCoordinatorLeaseSSHPort(lease, b.cfg)
	if err != nil {
		return statusView{}, err
	}
	server, target, leaseID := leaseToServerTarget(lease, b.cfg)
	resolved, err := resolveNetworkTarget(ctx, b.cfg, server, target)
	if err != nil {
		return statusView{}, err
	}
	target = resolved.Target
	hasHost := lease.Host != ""
	ready := false
	if lease.State == "active" && hasHost {
		if err := prepareLeaseSSHTrust(&target, leaseID); err != nil {
			return statusView{}, err
		}
		ready = probeSSHReady(ctx, &target, 4*time.Second)
	}
	return statusView{
		ID:                   lease.ID,
		Slug:                 lease.Slug,
		Provider:             blank(lease.Provider, b.cfg.Provider),
		TargetOS:             blank(target.TargetOS, b.cfg.TargetOS),
		WindowsMode:          blank(target.WindowsMode, b.cfg.WindowsMode),
		State:                lease.State,
		ServerID:             leaseDisplayID(lease),
		ServerType:           lease.ServerType,
		Host:                 lease.Host,
		Network:              resolved.Network,
		Tailscale:            lease.Tailscale,
		SSHHost:              target.Host,
		SSHHostKey:           target.SSHHostKey,
		SSHUser:              redactedSSHUser(b.cfg, server, target),
		SSHPort:              target.Port,
		SSHFallbackPorts:     target.FallbackPorts,
		SSHKey:               target.Key,
		LastTouchedAt:        lease.LastTouchedAt,
		IdleFor:              idleForString(lease.LastTouchedAt, time.Now()),
		IdleTimeout:          formatSecondsDuration(lease.IdleTimeoutSeconds),
		ExpiresAt:            lease.ExpiresAt,
		CleanupStatus:        lease.CleanupStatus,
		ProviderCleanup:      lease.ProviderCleanup,
		CleanupStartedAt:     lease.CleanupStartedAt,
		CleanupError:         lease.CleanupError,
		CleanupRetryAt:       lease.CleanupRetryAt,
		ReleaseDeletesServer: lease.ReleaseDeletesServer,
		Labels:               cloneStringMap(server.Labels),
		HasHost:              hasHost,
		Ready:                ready,
		Telemetry:            lease.Telemetry,
		TelemetryHistory:     lease.TelemetryHistory,
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
		if canonicalProvidersMatch(provider, lease.Provider) {
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
	_, err := b.ReleaseLeaseWithOutcome(ctx, req)
	return err
}

func (b *coordinatorLeaseBackend) ReleaseLeaseWithOutcome(ctx context.Context, req ReleaseLeaseRequest) (ReleaseLeaseOutcome, error) {
	var outcome ReleaseLeaseOutcome
	err := b.releaseLease(ctx, req, &outcome)
	return outcome, err
}

func (b *coordinatorLeaseBackend) releaseLease(ctx context.Context, req ReleaseLeaseRequest, outcome *ReleaseLeaseOutcome) error {
	if req.Lease.LeaseID == "" {
		return exit(2, "missing coordinator lease id")
	}
	if err := ValidateLeaseTargetProviderIdentity(req.Lease, req.ExpectedProviderIdentity); err != nil {
		return err
	}
	claim, exists, err := readLeaseClaimWithPresence(req.Lease.LeaseID)
	if err != nil {
		return err
	}
	return finalizeLeaseClaimIfUnchangedAfterContext(ctx, req.Lease.LeaseID, claim, exists, func() (bool, error) {
		authority := claim
		if !exists {
			authority.LeaseID = req.Lease.LeaseID
		}
		if err := AuthorizeCheckpointRelease(authority, req.CheckpointID); err != nil {
			return false, err
		}
		return b.releaseLeaseUnderClaimFence(ctx, req, outcome)
	}, syncControllerDirectory)
}

func (b *coordinatorLeaseBackend) releaseLeaseUnderClaimFence(ctx context.Context, req ReleaseLeaseRequest, outcome *ReleaseLeaseOutcome) (bool, error) {
	if req.Lease.LeaseID == "" {
		return false, exit(2, "missing coordinator lease id")
	}
	expectedProvider, err := b.expectedProvider()
	if err != nil {
		return false, err
	}
	if err := validateCoordinatorProviderIdentity(expectedProvider, req.Lease.LeaseID, req.Lease.Server.Provider, false); err != nil {
		return false, err
	}
	finish := func() (bool, error) {
		outcome.Terminal = true
		err := cleanupReleasedCoordinatorLeaseArtifacts(ctx, b.rt.Stderr, req.Lease.LeaseID)
		return err == nil, err
	}
	// A confirmed deletion is irreversible. Retrying retained local cleanup must
	// not repeat provider deletion, nor recreate guest trust or connections.
	if req.Lease.providerReleaseConfirmedBy(b) {
		return finish()
	}
	// Refresh old guest targets under the release fence. No endpoint means no
	// remote cleanup, so a failed explicit-stop lookup must not be repeated.
	if req.GuardedRemoteCleanup != nil && req.Lease.SSH.Host != "" {
		fresh, inspectErr := b.Resolve(ctx, ResolveRequest{ID: req.Lease.LeaseID, ReleaseOnly: true})
		if cause := context.Cause(ctx); cause != nil {
			return false, cause
		}
		if isCoordinatorProviderIdentityError(inspectErr) {
			return false, inspectErr
		}
		if inspectErr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: could not inspect lease before release: %v\n", inspectErr)
		} else {
			if err := ValidateLeaseTargetProviderIdentity(fresh, ProviderIdentityExpectation{LeaseID: req.Lease.LeaseID}); err != nil {
				return false, err
			}
			if err := ValidateLeaseTargetProviderIdentity(fresh, req.ExpectedProviderIdentity); err != nil {
				return false, err
			}
			if fresh.providerReleaseConfirmedBy(b) {
				return finish()
			}
			if fresh.SSH.Host != "" {
				// Route probes are guest cleanup too; they must not consume the
				// provider-release budget or replace a tailnet route with the public host.
				cleanupCtx, cancel := context.WithTimeout(ctx, remoteConnectionCleanupTimeout)
				resolved, routeErr := resolveNetworkTarget(cleanupCtx, b.cfg, fresh.Server, fresh.SSH)
				if routeErr != nil {
					fmt.Fprintf(b.rt.Stderr, "warning: could not resolve guest network before release: %v\n", routeErr)
				} else {
					fresh.SSH = resolved.Target
					req.GuardedRemoteCleanup(cleanupCtx, fresh)
				}
				cancel()
			}
		}
	}
	released, err := releaseCoordinatorLeaseResult(ctx, b.coord, req.Lease.LeaseID, expectedProvider)
	observationCoord := b.coord
	if err != nil {
		if b.cfg.CoordAdminToken != "" && (isCoordinatorNotFoundError(err) || isCoordinatorUnauthorized(err)) {
			adminCoord, adminErr := b.adminCoordinatorClient()
			if adminErr != nil {
				return false, err
			}
			if released, adminErr = releaseCoordinatorLeaseMutation(ctx, req.Lease.LeaseID, expectedProvider, func(releaseCtx context.Context) (CoordinatorLease, error) {
				return adminCoord.AdminReleaseLeaseForProvider(releaseCtx, req.Lease.LeaseID, true, expectedProvider)
			}); adminErr != nil {
				return false, adminErr
			}
			observationCoord = adminCoord
		} else {
			return false, err
		}
	}
	if req.DeferProviderCleanupObservation {
		if retainedCoordinatorRelease(released) {
			return false, nil
		}
		if coordinatorProviderReleaseConfirmed(released) {
			return finish()
		}
		if coordinatorReleaseCleanupFailed(released) || coordinatorReleaseCleanupPending(released) {
			fmt.Fprintf(b.rt.Stderr, "warning: coordinator accepted release for %s; remote cleanup remains pending and local claim/SSH artifacts were preserved\n", req.Lease.LeaseID)
			return false, nil
		}
		return false, coordinatorReleaseObservationError(req.Lease.LeaseID, "returned an unexpected non-final state")
	}
	released, err = observeCoordinatorReleaseCompletion(ctx, observationCoord, released, req.Lease.LeaseID, expectedProvider)
	if err != nil {
		return false, err
	}
	if retainedCoordinatorRelease(released) {
		return false, nil
	}
	if coordinatorProviderReleaseConfirmed(released) {
		return finish()
	}
	return true, nil
}

func (b *coordinatorLeaseBackend) CheckpointSourceAbsent(ctx context.Context, req CheckpointSourceRequest) (bool, error) {
	lease, err := b.coord.GetLease(ctx, req.LeaseID)
	if err != nil && b.cfg.CoordAdminToken != "" && (isCoordinatorNotFoundError(err) || isCoordinatorUnauthorized(err)) {
		adminCoord, adminErr := b.adminCoordinatorClient()
		if adminErr == nil {
			lease, adminErr = adminCoord.GetLease(ctx, req.LeaseID)
		}
		if adminErr == nil {
			err = nil
		}
	}
	if err != nil {
		return false, err
	}
	if lease.ID != req.LeaseID || lease.Provider != req.Resource.Image.Provider || lease.CloudID != req.Capture.SourceID {
		return false, exit(2, "coordinator checkpoint source identity changed")
	}
	return coordinatorProviderReleaseConfirmed(lease), nil
}

func (b *coordinatorLeaseBackend) adminCoordinatorClient() (*CoordinatorClient, error) {
	cfg := b.cfg
	cfg.CoordToken = cfg.CoordAdminToken
	cfg.CoordTokenCommand = nil
	coord, _, err := newCoordinatorClient(cfg)
	return coord, err
}

func (b *coordinatorLeaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	expectedProvider, err := b.expectedProvider()
	if err != nil {
		return req.Lease.Server, err
	}
	if err := validateCoordinatorProviderIdentity(expectedProvider, req.Lease.LeaseID, req.Lease.Server.Provider, false); err != nil {
		return req.Lease.Server, err
	}
	lease, err := b.coord.TouchLeaseForProvider(ctx, req.Lease.LeaseID, expectedProvider)
	if err != nil {
		return req.Lease.Server, err
	}
	if err := b.validateCoordinatorLeaseProviderIdentity(lease); err != nil {
		return req.Lease.Server, err
	}
	server, _, _ := leaseToServerTarget(lease, b.cfg)
	return server, nil
}

// HeartbeatLease sends one owner-scoped coordinator heartbeat and returns the
// committed lease state. A non-nil idle timeout uses the existing heartbeat
// override field; ordinary touches intentionally leave that field unset.
func (b *coordinatorLeaseBackend) HeartbeatLease(ctx context.Context, leaseID string, idleTimeout *time.Duration) (CoordinatorLease, error) {
	expectedProvider, err := b.expectedProvider()
	if err != nil {
		return CoordinatorLease{}, err
	}
	lease, err := heartbeatCoordinatorLease(ctx, b.coord, leaseID, expectedProvider, idleTimeout)
	if err != nil {
		return CoordinatorLease{}, err
	}
	if err := b.validateCoordinatorLeaseProviderIdentity(lease); err != nil {
		return CoordinatorLease{}, err
	}
	return lease, nil
}

func heartbeatCoordinatorLease(ctx context.Context, coord *CoordinatorClient, leaseID, expectedProvider string, idleTimeout *time.Duration) (CoordinatorLease, error) {
	if idleTimeout != nil {
		return coord.UpdateLeaseIdleTimeoutForProvider(ctx, leaseID, expectedProvider, *idleTimeout)
	}
	return coord.TouchLeaseForProvider(ctx, leaseID, expectedProvider)
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
