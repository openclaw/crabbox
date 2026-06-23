package agentsandbox

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

type backend struct {
	spec        ProviderSpec
	cfg         Config
	rt          Runtime
	newClient   func(context.Context, Config, Runtime) (kubernetesClient, error)
	removeClaim func(string, LeaseClaim) error
}

type claimTTLExpiryError struct {
	err error
}

type ambiguousClaimRecoveryUnknownError struct {
	err error
}

func (e claimTTLExpiryError) Error() string {
	return e.err.Error()
}

func (e claimTTLExpiryError) Unwrap() error {
	return e.err
}

func (e ambiguousClaimRecoveryUnknownError) Error() string {
	return e.err.Error()
}

func (e ambiguousClaimRecoveryUnknownError) Unwrap() error {
	return e.err
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	client, err := b.client(ctx)
	if err != nil {
		return DoctorResult{}, err
	}
	checks, err := b.doctorChecks(ctx, client)
	result := DoctorResult{
		Provider: providerName,
		Status:   "ready",
		Checks:   checks,
		Message: fmt.Sprintf("kubernetes=ready crds=ready rbac=ready warm_pool=%s namespace=%s context=%s mutation=false",
			b.cfg.AgentSandbox.WarmPool, b.cfg.AgentSandbox.Namespace, b.cfg.AgentSandbox.Context),
	}
	if err != nil {
		result.Status = "blocked"
		result.Message = err.Error()
		return result, err
	}
	return result, nil
}

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	started := b.now()
	client, err := b.client(ctx)
	if err != nil {
		return err
	}
	leaseID, claimName, slug, ready, claim, unlockOperation, err := b.createClaim(ctx, client, req.RequestedSlug, req.Repo, req.Reclaim)
	if err != nil {
		return err
	}
	defer unlockOperation()
	total := b.now().Sub(started)
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s claim=%s sandbox=%s pod=%s\n", leaseID, slug, providerName, claimName, ready.SandboxName, ready.PodName)
	if !req.Keep {
		if expiresAt := strings.TrimSpace(claim.Labels[claimLabelExpiresAt]); expiresAt != "" {
			fmt.Fprintf(b.rt.Stderr, "warning: agent-sandbox warmup keeps the claim until explicit stop or ttl expiry=%s\n", expiresAt)
		} else {
			fmt.Fprintf(b.rt.Stderr, "warning: agent-sandbox warmup keeps the claim until explicit stop\n")
		}
	}
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", total.Round(time.Millisecond))
	if req.TimingJSON {
		return writeTimingJSON(b.rt.Stderr, timingReport{Provider: providerName, LeaseID: leaseID, Slug: slug, TotalMs: total.Milliseconds(), ExitCode: 0})
	}
	return nil
}

func (b *backend) Run(ctx context.Context, req RunRequest) (result RunResult, retErr error) {
	if req.Options.Tailscale.Enabled {
		return RunResult{}, exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	workdir := path.Clean(b.cfg.AgentSandbox.Workdir)
	started := b.now()
	client, err := b.client(ctx)
	if err != nil {
		return RunResult{}, err
	}
	leaseID, slug, claimName := "", "", ""
	ready := sandboxReadiness{}
	claim := LeaseClaim{}
	acquired := false
	shouldStop := false
	var unlockOperation func()
	defer func() {
		if unlockOperation != nil {
			unlockOperation()
		}
	}()
	if req.ID == "" {
		leaseID, claimName, slug, ready, claim, unlockOperation, err = b.createClaim(ctx, client, req.RequestedSlug, req.Repo, req.Reclaim)
		if err != nil {
			return RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s claim=%s sandbox=%s pod=%s\n", leaseID, slug, providerName, claimName, ready.SandboxName, ready.PodName)
		acquired = true
	} else {
		claim, err = resolveLocalClaim(req.ID)
		if err != nil {
			return RunResult{}, err
		}
		unlockOperation, err = lockAgentSandboxLeaseOperation(ctx, claim.LeaseID)
		if err != nil {
			return RunResult{}, err
		}
		claim, err = resolveLocalClaim(claim.LeaseID)
		if err != nil {
			return RunResult{}, err
		}
		if err := authorizeClaimScope(b.cfg, claim); err != nil {
			return RunResult{}, err
		}
		if err := authorizeAgentSandboxRepoClaim(claim, req.Repo.Root, req.Reclaim); err != nil {
			return RunResult{}, err
		}
		leaseID, slug, claimName = claim.LeaseID, claim.Slug, claimNameFromLocalClaim(claim)
		liveClaim, err := client.Get(ctx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName)
		if err != nil {
			if isNotFound(err) {
				return RunResult{}, b.missingClaimRunError(claim)
			}
			return RunResult{}, err
		}
		var identity claimIdentity
		claim, identity, err = b.claimIdentityForLiveClaim(claim, liveClaim, true)
		if err != nil {
			return RunResult{}, err
		}
		if expired, expiryErr := b.releaseExpiredRunClaim(ctx, client, claim, claimName); expired {
			return RunResult{}, expiryErr
		}
		ready, err = b.waitForClaimReadiness(ctx, client, claimName, identity)
		if err != nil {
			var ttlErr claimTTLExpiryError
			if errors.As(err, &ttlErr) {
				cleanupCtx, cancel := b.cleanupContext(ctx)
				defer cancel()
				if cleanupErr := b.deleteOwnedClaim(cleanupCtx, client, claim, claim.LeaseID, claimName, false); cleanupErr != nil {
					return RunResult{}, errors.Join(err, fmt.Errorf("release expired agent-sandbox claim %s: %w", claim.LeaseID, cleanupErr))
				}
				return RunResult{}, err
			}
			return RunResult{}, b.readinessRunError(ctx, client, claim, claimName, err)
		}
		if err := claimLeaseForRepo(b.cfg, claim.LeaseID, claim.Slug, req.Repo, req.Reclaim); err != nil {
			return RunResult{}, err
		}
		updated, err := readLeaseClaim(claim.LeaseID)
		if err != nil {
			return RunResult{}, err
		}
		claim, err = updateLeaseClaimLabelsIfUnchanged(claim.LeaseID, updated, claimReadinessLabels(claim.Labels, ready))
		if err != nil {
			return RunResult{}, err
		}
	}
	shouldStop = acquired && !req.Keep && b.cfg.AgentSandbox.DeleteOnRelease
	var pendingTiming *timingReport
	commandFailed := false
	providerFailure := false
	defer func() {
		if claim.LeaseID != "" && claimTTLExpired(claim, b.now().UTC()) {
			shouldStop = true
			providerFailure = true
			expiryErr := exit(1, "agent-sandbox claim %s reached its TTL expiry during the run", claim.LeaseID)
			if result.ExitCode == 0 {
				result.ExitCode = 1
			}
			if retErr == nil {
				retErr = expiryErr
			} else {
				retErr = errors.Join(retErr, expiryErr)
			}
		}
		if shouldStop {
			cleanupCtx, cancel := b.cleanupContext(ctx)
			defer cancel()
			if cleanupErr := b.deleteCurrentRunClaim(cleanupCtx, client, leaseID, claimName); cleanupErr != nil {
				if result.ExitCode == 0 {
					result.ExitCode = 1
				}
				if !commandFailed {
					providerFailure = true
				}
				if retErr == nil {
					retErr = exit(1, "%v", cleanupErr)
				} else {
					retErr = errors.Join(retErr, cleanupErr)
				}
			}
		}
		if pendingTiming != nil {
			pendingTiming.ExitCode = result.ExitCode
			if result.Total > 0 {
				pendingTiming.TotalMs = result.Total.Milliseconds()
			}
			report := timingReportWithRunResult(*pendingTiming, result, retErr)
			if providerFailure {
				report = timingReportWithProviderError(report)
			}
			_ = writeTimingJSON(b.rt.Stderr, report)
		}
	}()
	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s claim=%s sandbox=%s pod=%s workdir=%s\n", providerName, leaseID, claimName, ready.SandboxName, ready.PodName, workdir)

	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, client, ready, req, workdir)
		if err != nil {
			handleDelegatedRunFailure(b.rt.Stderr, b.cfg, req, leaseID, slug, acquired, &shouldStop)
			result := RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true}
			return result, b.refreshRetainedFailureActivity(claim, leaseID, shouldStop, err)
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.execShell(ctx, client, ready, "mkdir -p "+shellQuote(workdir)); err != nil {
		handleDelegatedRunFailure(b.rt.Stderr, b.cfg, req, leaseID, slug, acquired, &shouldStop)
		result := RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true}
		return result, b.refreshRetainedFailureActivity(claim, leaseID, shouldStop, err)
	}
	if claimTTLExpired(claim, b.now().UTC()) {
		shouldStop = true
		result := RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true}
		return result, exit(4, "agent-sandbox claim %s reached its TTL expiry; command not run", claim.LeaseID)
	}
	if req.SyncOnly {
		result = RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workdir)
		if !shouldStop {
			if err := refreshClaimLeaseActivity(b.cfg, claim); err != nil && claim.LeaseID != "" {
				fmt.Fprintf(b.rt.Stderr, "warning: refresh agent-sandbox lease activity failed lease=%s: %v\n", leaseID, err)
				result.ExitCode = 1
				providerFailure = true
			}
		}
		if req.TimingJSON {
			pendingTiming = &timingReport{Provider: providerName, LeaseID: leaseID, Slug: slug, SyncDelegated: true, SyncMs: syncDuration.Milliseconds(), SyncPhases: syncPhases, SyncSkipped: req.NoSync, TotalMs: result.Total.Milliseconds(), ExitCode: result.ExitCode, Label: strings.TrimSpace(req.Label)}
		}
		if result.ExitCode != 0 {
			return result, exit(result.ExitCode, "agent-sandbox sync-only completed with warnings")
		}
		return result, nil
	}
	commandStart := b.now()
	exitCode, runErr := b.runCommand(ctx, client, ready, req, workdir)
	commandDuration := b.now().Sub(commandStart)
	result = RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, ExitCode: exitCode, Command: commandDuration, Total: b.now().Sub(started), SyncDelegated: true}
	fmt.Fprintf(b.rt.Stderr, "agent-sandbox run summary sync=%s command=%s total=%s exit=%d\n", syncDuration.Round(time.Millisecond), commandDuration.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	var commandErr error
	if runErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, b.cfg, req, leaseID, slug, acquired, &shouldStop)
		commandErr = runErr
	} else if exitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, b.cfg, req, leaseID, slug, acquired, &shouldStop)
		commandErr = exit(exitCode, "agent-sandbox run exited %d", exitCode)
	}
	commandFailed = commandErr != nil
	if !shouldStop {
		if err := refreshClaimLeaseActivity(b.cfg, claim); err != nil && claim.LeaseID != "" {
			fmt.Fprintf(b.rt.Stderr, "warning: refresh agent-sandbox lease activity failed lease=%s: %v\n", leaseID, err)
			if commandErr == nil {
				result.ExitCode = 1
				commandErr = err
				providerFailure = true
			} else {
				commandErr = errors.Join(commandErr, fmt.Errorf("refresh agent-sandbox lease activity: %w", err))
			}
		}
	}
	if req.TimingJSON {
		pendingTiming = &timingReport{Provider: providerName, LeaseID: leaseID, Slug: slug, SyncDelegated: true, SyncMs: syncDuration.Milliseconds(), SyncPhases: syncPhases, SyncSkipped: req.NoSync, CommandMs: commandDuration.Milliseconds(), TotalMs: result.Total.Milliseconds(), ExitCode: result.ExitCode, Label: strings.TrimSpace(req.Label)}
	}
	return result, commandErr
}

func (b *backend) deleteCurrentRunClaim(ctx context.Context, client kubernetesClient, leaseID, claimName string) error {
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return fmt.Errorf("read agent-sandbox lease %s before release: %w", leaseID, err)
	}
	if claim.LeaseID == "" {
		return exit(4, "agent-sandbox lease %s disappeared before release", leaseID)
	}
	return b.deleteOwnedClaim(ctx, client, claim, leaseID, claimName, false)
}

func (b *backend) refreshRetainedFailureActivity(claim LeaseClaim, leaseID string, shouldStop bool, cause error) error {
	if shouldStop || claim.LeaseID == "" {
		return cause
	}
	if err := refreshClaimLeaseActivity(b.cfg, claim); err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: refresh agent-sandbox lease activity failed lease=%s: %v\n", leaseID, err)
		return errors.Join(cause, fmt.Errorf("refresh agent-sandbox lease activity: %w", err))
	}
	return cause
}

func (b *backend) releaseExpiredRunClaim(ctx context.Context, client kubernetesClient, claim LeaseClaim, claimName string) (bool, error) {
	if !claimTTLExpired(claim, b.now().UTC()) {
		return false, nil
	}
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	cause := exit(4, "agent-sandbox claim %s reached its TTL expiry; command not run", claim.LeaseID)
	if err := b.deleteOwnedClaim(cleanupCtx, client, claim, claim.LeaseID, claimName, false); err != nil {
		return true, errors.Join(cause, fmt.Errorf("release expired agent-sandbox claim %s: %w", claim.LeaseID, err))
	}
	return true, cause
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	client, err := b.client(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider != providerName || claim.ProviderScope != claimScope(b.cfg) {
			continue
		}
		claimName := claimNameFromLocalClaim(claim)
		ready := sandboxReadiness{}
		state := statusViewReady
		stateReason := ""
		liveClaim, stateErr := client.Get(ctx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName)
		if stateErr == nil {
			_, identity, err := b.claimIdentityForLiveClaim(claim, liveClaim, false)
			if err != nil {
				return nil, err
			}
			if expired, reason := sandboxClaimExpired(claim, liveClaim, b.now().UTC()); expired {
				state = "expired"
				stateReason = reason
			} else {
				ready, stateErr = sandboxReadinessOnce(ctx, client, b.cfg.AgentSandbox.Namespace, claimName, identity)
			}
		}
		if stateErr != nil {
			if isNotFound(stateErr) {
				state = "missing-or-inaccessible"
			} else if errors.Is(stateErr, errNotReady) {
				state = "not-ready"
			} else if isSandboxExpiredError(stateErr) {
				state = "expired"
			} else if isResourceTerminalError(stateErr) {
				state = "failed"
			} else {
				return nil, stateErr
			}
			stateReason = stateErr.Error()
		}
		labels := map[string]string{
			"provider":  providerName,
			"lease":     claim.LeaseID,
			"slug":      claim.Slug,
			"pond":      claim.Pond,
			"target":    targetLinux,
			"state":     state,
			"namespace": b.cfg.AgentSandbox.Namespace,
			"warm_pool": b.cfg.AgentSandbox.WarmPool,
			"claim":     claimName,
			"sandbox":   ready.SandboxName,
			"pod":       ready.PodName,
		}
		if stateReason != "" {
			labels["reason"] = stateReason
		}
		servers = append(servers, Server{Provider: providerName, CloudID: claimName, Name: claimName, Status: state, Labels: labels})
	}
	return servers, nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	client, err := b.client(ctx)
	if err != nil {
		return StatusView{}, err
	}
	claim, err := resolveLocalClaim(req.ID)
	if err != nil {
		return StatusView{}, err
	}
	if err := authorizeClaimScope(b.cfg, claim); err != nil {
		return StatusView{}, err
	}
	waitTimeout := req.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = 5 * time.Minute
	}
	pollCtx := ctx
	cancel := func() {}
	if req.Wait {
		pollCtx, cancel = context.WithTimeout(ctx, waitTimeout)
	}
	defer cancel()
	claimName := claimNameFromLocalClaim(claim)
	baseView := StatusView{ID: claim.LeaseID, Slug: claim.Slug, Provider: providerName, TargetOS: targetLinux, ServerID: claimName, Pond: claim.Pond, Network: networkPublic, Labels: map[string]string{
		"provider":  providerName,
		"lease":     claim.LeaseID,
		"pond":      claim.Pond,
		"claim":     claimName,
		"namespace": b.cfg.AgentSandbox.Namespace,
		"warm_pool": b.cfg.AgentSandbox.WarmPool,
	}}
	for {
		liveClaim, err := client.Get(pollCtx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName)
		if err != nil {
			if isNotFound(err) {
				baseView.State = "missing-or-inaccessible"
				baseView.Labels["reason"] = err.Error()
				if req.Wait {
					return StatusView{}, exit(4, "agent-sandbox claim %s is missing in Kubernetes", claimName)
				}
				return baseView, nil
			}
			return StatusView{}, err
		}
		_, identity, err := b.claimIdentityForLiveClaim(claim, liveClaim, false)
		if err != nil {
			return StatusView{}, err
		}
		if expired, reason := sandboxClaimExpired(claim, liveClaim, b.now().UTC()); expired {
			view := baseView
			view.State = "expired"
			view.Labels = cloneStringMap(baseView.Labels)
			view.Labels["reason"] = reason
			return view, nil
		}
		ready, readyErr := sandboxReadinessOnce(pollCtx, client, b.cfg.AgentSandbox.Namespace, claimName, identity)
		view := baseView
		view.Labels = cloneStringMap(baseView.Labels)
		if readyErr == nil {
			view.State = statusViewReady
			view.Ready = true
			view.Labels["sandbox"] = ready.SandboxName
			view.Labels["pod"] = ready.PodName
			view.Labels["pod_ip"] = ready.PodIP
			return view, nil
		}
		if errors.Is(readyErr, errSandboxClaimNotFound) {
			view.State = "missing-or-inaccessible"
			view.Labels["reason"] = readyErr.Error()
			if req.Wait {
				return StatusView{}, exit(4, "agent-sandbox claim %s is missing in Kubernetes", claimName)
			}
			return view, nil
		}
		if isSandboxExpiredError(readyErr) {
			view.State = "expired"
			view.Labels["reason"] = readyErr.Error()
			return view, nil
		}
		if isResourceTerminalError(readyErr) {
			view.State = "failed"
			view.Labels["reason"] = readyErr.Error()
			return view, nil
		}
		if !errors.Is(readyErr, errNotReady) && !isNotFound(readyErr) {
			return StatusView{}, readyErr
		}
		view.State = "not-ready"
		if isNotFound(readyErr) {
			view.State = "missing-or-inaccessible"
		}
		view.Labels["reason"] = readyErr.Error()
		if !req.Wait {
			return view, nil
		}
		select {
		case <-pollCtx.Done():
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for agent-sandbox claim %s to become ready", claimName)
			}
			return StatusView{}, pollCtx.Err()
		case <-time.After(agentSandboxStatusPoll):
		}
	}
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	client, err := b.client(ctx)
	if err != nil {
		return err
	}
	claim, err := resolveLocalClaim(req.ID)
	if err != nil {
		return err
	}
	unlockOperation, err := lockAgentSandboxLeaseOperation(ctx, claim.LeaseID)
	if err != nil {
		return err
	}
	defer unlockOperation()
	claim, err = resolveLocalClaim(claim.LeaseID)
	if err != nil {
		return err
	}
	if err := authorizeClaimScope(b.cfg, claim); err != nil {
		return err
	}
	claimName := claimNameFromLocalClaim(claim)
	if err := b.deleteOwnedClaim(ctx, client, claim, claim.LeaseID, claimName, true); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s claim=%s\n", claim.LeaseID, claimName)
	return nil
}

func (b *backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	client, err := b.client(ctx)
	if err != nil {
		return err
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		return err
	}
	now := b.now().UTC()
	checked, removed, claimsRemoved := 0, 0, 0
	for _, listedClaim := range claims {
		if listedClaim.Provider != providerName || listedClaim.ProviderScope != claimScope(b.cfg) {
			continue
		}
		var checkedOne, removedOne, claimRemovedOne bool
		err := func() error {
			unlockOperation, err := lockAgentSandboxLeaseOperation(ctx, listedClaim.LeaseID)
			if err != nil {
				return err
			}
			defer unlockOperation()
			claim, err := readLeaseClaim(listedClaim.LeaseID)
			if err != nil {
				return err
			}
			if claim.LeaseID == "" || claim.Provider != providerName || claim.ProviderScope != claimScope(b.cfg) {
				return nil
			}
			checkedOne = true
			claimName := claimNameFromLocalClaim(claim)
			liveClaim, getErr := client.Get(ctx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName)
			if getErr != nil {
				if !isNotFound(getErr) {
					return getErr
				}
				if !b.cfg.AgentSandbox.ForgetMissing {
					fmt.Fprintf(b.rt.Stderr, "skip claim=%s lease=%s reason=missing-or-inaccessible; set agentSandbox forgetMissing to remove the local claim\n", claimName, claim.LeaseID)
					return nil
				}
				if req.DryRun {
					fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s slug=%s reason=missing claim\n", claim.LeaseID, blank(claim.Slug, "-"))
					return nil
				}
				if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
					return err
				}
				fmt.Fprintf(b.rt.Stdout, "remove claim lease=%s slug=%s reason=missing claim\n", claim.LeaseID, blank(claim.Slug, "-"))
				claimRemovedOne = true
				return nil
			}
			claim, _, err = b.claimIdentityForLiveClaim(claim, liveClaim, !req.DryRun)
			if err != nil {
				return err
			}
			due, reason := claimCleanupDue(claim, now)
			if !due {
				fmt.Fprintf(b.rt.Stderr, "skip claim=%s lease=%s reason=%s\n", claimName, claim.LeaseID, reason)
				return nil
			}
			if req.DryRun {
				fmt.Fprintf(b.rt.Stdout, "would delete claim=%s lease=%s reason=%s\n", claimName, claim.LeaseID, reason)
				return nil
			}
			if err := b.deleteOwnedClaim(ctx, client, claim, claim.LeaseID, claimName, false); err != nil {
				return err
			}
			fmt.Fprintf(b.rt.Stdout, "delete claim=%s lease=%s reason=%s\n", claimName, claim.LeaseID, reason)
			removedOne = true
			return nil
		}()
		if err != nil {
			return err
		}
		if checkedOne {
			checked++
		}
		if removedOne {
			removed++
		}
		if claimRemovedOne {
			claimsRemoved++
		}
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d claims_removed=%d checked=%d\n", providerName, removed, claimsRemoved, checked)
	}
	return nil
}

func (b *backend) createClaim(ctx context.Context, client kubernetesClient, requestedSlug string, repo Repo, reclaim bool) (
	string, string, string, sandboxReadiness, LeaseClaim, func(), error,
) {
	unlockSlug, err := lockAgentSandboxSlugAllocation(ctx, requestedSlug)
	if err != nil {
		return "", "", "", sandboxReadiness{}, LeaseClaim{}, nil, err
	}
	defer func() {
		if unlockSlug != nil {
			unlockSlug()
		}
	}()
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", "", "", sandboxReadiness{}, LeaseClaim{}, nil, err
	}
	unlockOperation, err := lockAgentSandboxLeaseOperation(ctx, leaseID)
	if err != nil {
		return "", "", "", sandboxReadiness{}, LeaseClaim{}, nil, err
	}
	createdSuccessfully := false
	defer func() {
		if !createdSuccessfully {
			unlockOperation()
		}
	}()
	claimResourceName := claimName(leaseID, slug)
	recoveryNonce, err := newClaimRecoveryNonce()
	if err != nil {
		return "", "", "", sandboxReadiness{}, LeaseClaim{}, nil, err
	}
	expiresAt := ""
	if b.cfg.TTL > 0 {
		deadline := b.now().UTC().Add(b.cfg.TTL)
		if deadline.Nanosecond() != 0 {
			deadline = deadline.Truncate(time.Second).Add(time.Second)
		}
		expiresAt = deadline.Format(time.RFC3339)
	}
	spec := map[string]any{
		"warmPoolRef": map[string]any{"name": b.cfg.AgentSandbox.WarmPool},
	}
	if expiresAt != "" {
		spec["lifecycle"] = map[string]any{
			"shutdownPolicy": "Retain",
			"shutdownTime":   expiresAt,
		}
	}
	obj := &kubernetesObject{
		APIVersion: agentSandboxExtensionsGroupVersion,
		Kind:       "SandboxClaim",
		Metadata: objectMeta{
			Name:        claimResourceName,
			Namespace:   b.cfg.AgentSandbox.Namespace,
			Labels:      claimLabels(leaseID, slug),
			Annotations: claimAnnotationsWithRecoveryNonce(b.cfg, recoveryNonce),
		},
		Spec: spec,
	}
	created, err := client.Create(ctx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, obj)
	if err != nil && !createMayHaveSucceeded(err) {
		return "", "", "", sandboxReadiness{}, LeaseClaim{}, nil, err
	}
	if err != nil || created == nil || strings.TrimSpace(created.Metadata.UID) == "" {
		cause := err
		if cause == nil {
			cause = exit(4, "created agent-sandbox claim %s has no Kubernetes UID", claimResourceName)
		}
		created, err = b.reconcileCreatedClaim(ctx, client, leaseID, claimResourceName, expiresAt, recoveryNonce, cause)
		if err != nil {
			return "", "", "", sandboxReadiness{}, LeaseClaim{}, nil, b.recoverAmbiguousCreateFailure(
				leaseID, slug, repo, reclaim, claimResourceName, expiresAt, recoveryNonce, err,
			)
		}
		fmt.Fprintf(b.rt.Stderr, "warning: reconciled agent-sandbox claim=%s after ambiguous create result\n", claimResourceName)
	}
	identity := claimIdentity{
		LeaseID:       leaseID,
		ProviderScope: claimScope(b.cfg),
		UID:           strings.TrimSpace(created.Metadata.UID),
		WarmPool:      b.cfg.AgentSandbox.WarmPool,
		ExpiresAt:     expiresAt,
		Container:     strings.TrimSpace(b.cfg.AgentSandbox.Container),
	}
	pending := sandboxReadiness{ClaimName: claimResourceName, ClaimUID: identity.UID}
	pendingClaim, err := writeClaimLease(b.cfg, leaseID, slug, repo, reclaim, pending, claimResourceName, expiresAt, recoveryNonce)
	if err != nil {
		return "", "", "", sandboxReadiness{}, LeaseClaim{}, nil, b.rollbackCreatedClaim(client, leaseID, slug, repo, reclaim, claimResourceName, pending, expiresAt, recoveryNonce, err)
	}
	unlockSlug()
	unlockSlug = nil
	ready, err := b.waitForClaimReadiness(ctx, client, claimResourceName, identity)
	if err != nil {
		return "", "", "", sandboxReadiness{}, LeaseClaim{}, nil, b.rollbackCreatedClaim(client, leaseID, slug, repo, reclaim, claimResourceName, pending, expiresAt, recoveryNonce, err)
	}
	if claimTTLExpired(pendingClaim, b.now().UTC()) {
		cause := exit(4, "agent-sandbox claim %s reached its TTL expiry before becoming ready", leaseID)
		return "", "", "", sandboxReadiness{}, LeaseClaim{}, nil, b.rollbackCreatedClaim(client, leaseID, slug, repo, reclaim, claimResourceName, pending, expiresAt, recoveryNonce, cause)
	}
	persistedClaim, err := writeClaimLease(b.cfg, leaseID, slug, repo, reclaim, ready, claimResourceName, expiresAt, recoveryNonce)
	if err != nil {
		return "", "", "", sandboxReadiness{}, LeaseClaim{}, nil, b.rollbackCreatedClaim(client, leaseID, slug, repo, reclaim, claimResourceName, pending, expiresAt, recoveryNonce, err)
	}
	createdSuccessfully = true
	return leaseID, claimResourceName, slug, ready, persistedClaim, unlockOperation, nil
}

func (b *backend) waitForClaimReadiness(ctx context.Context, client kubernetesClient, claimName string, identity claimIdentity) (sandboxReadiness, error) {
	readinessCtx := ctx
	cancel := func() {}
	ttlBounded := false
	if identity.ExpiresAt != "" {
		expiresAt, err := time.Parse(time.RFC3339, identity.ExpiresAt)
		if err != nil {
			return sandboxReadiness{}, exit(4, "agent-sandbox claim %s has invalid TTL expiry %q", identity.LeaseID, identity.ExpiresAt)
		}
		remaining := expiresAt.Sub(b.now().UTC())
		if remaining <= 0 {
			return sandboxReadiness{}, claimTTLExpiryError{err: exit(4, "agent-sandbox claim %s reached its TTL expiry before becoming ready", identity.LeaseID)}
		}
		readinessCtx, cancel = context.WithTimeout(ctx, remaining)
		ttlBounded = true
	}
	defer cancel()
	ready, err := waitForSandboxReadinessWithTimeouts(
		readinessCtx,
		client,
		b.cfg.AgentSandbox.Namespace,
		claimName,
		identity,
		readinessTimeout(b.cfg),
		podReadinessTimeout(b.cfg),
		time.Second,
	)
	if err != nil && ttlBounded && errors.Is(readinessCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return sandboxReadiness{}, claimTTLExpiryError{err: exit(4, "agent-sandbox claim %s reached its TTL expiry before becoming ready", identity.LeaseID)}
	}
	return ready, err
}

func (b *backend) reconcileCreatedClaim(ctx context.Context, client kubernetesClient, leaseID, claimName, expiresAt, recoveryNonce string, cause error) (*kubernetesObject, error) {
	reconcileCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		live, err := client.Get(reconcileCtx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName)
		if err == nil {
			identity := claimIdentity{
				LeaseID:       leaseID,
				ProviderScope: claimScope(b.cfg),
				UID:           strings.TrimSpace(live.Metadata.UID),
				WarmPool:      b.cfg.AgentSandbox.WarmPool,
				ExpiresAt:     expiresAt,
				Container:     strings.TrimSpace(b.cfg.AgentSandbox.Container),
			}
			if identity.UID == "" {
				return nil, errors.Join(cause, exit(4, "reconciled agent-sandbox claim %s has no Kubernetes UID", claimName))
			}
			if err := validateClaimIdentity(live, identity); err != nil {
				return nil, errors.Join(cause, fmt.Errorf("refuse ambiguous agent-sandbox claim recovery: %w", err))
			}
			if err := validateClaimRecoveryNonce(live, recoveryNonce); err != nil {
				return nil, errors.Join(cause, fmt.Errorf("refuse ambiguous agent-sandbox claim recovery: %w", err))
			}
			return live, nil
		}
		lastErr = err
		select {
		case <-reconcileCtx.Done():
			if isNotFound(lastErr) {
				return nil, cause
			}
			return nil, ambiguousClaimRecoveryUnknownError{err: errors.Join(
				cause,
				fmt.Errorf("reconcile agent-sandbox claim %s/%s after ambiguous create: %w", b.cfg.AgentSandbox.Namespace, claimName, lastErr),
			)}
		case <-ticker.C:
		}
	}
}

func (b *backend) rollbackCreatedClaim(
	client kubernetesClient,
	leaseID, slug string,
	repo Repo,
	reclaim bool,
	claimName string,
	pending sandboxReadiness,
	expiresAt string,
	recoveryNonce string,
	cause error,
) error {
	cleanupCtx, cleanupCancel := b.cleanupContext(context.Background())
	defer cleanupCancel()
	cleanupErr := client.Delete(cleanupCtx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName, pending.ClaimUID)
	if cleanupErr == nil || isNotFound(cleanupErr) {
		claim, readErr := readLeaseClaim(leaseID)
		if readErr != nil {
			return errors.Join(cause, fmt.Errorf("read agent-sandbox recovery lease %s after rollback deletion: %w", leaseID, readErr))
		}
		if claim.LeaseID != "" {
			if removeErr := removeLeaseClaimIfUnchanged(leaseID, claim); removeErr != nil {
				return errors.Join(cause, fmt.Errorf("remove agent-sandbox recovery lease %s after rollback deletion: %w", leaseID, removeErr))
			}
		}
		return cause
	}
	if _, persistErr := writeClaimLease(b.cfg, leaseID, slug, repo, reclaim, pending, claimName, expiresAt, recoveryNonce); persistErr != nil {
		return errors.Join(
			cause,
			fmt.Errorf("failed to delete agent-sandbox claim %s/%s UID %s during rollback: %w", b.cfg.AgentSandbox.Namespace, claimName, pending.ClaimUID, cleanupErr),
			fmt.Errorf("failed to persist recovery lease %s; manual cleanup may be required: %w", leaseID, persistErr),
		)
	}
	stopCommand := agentSandboxRecoveryCommand(b.cfg, "stop")
	return errors.Join(
		cause,
		fmt.Errorf("failed to delete agent-sandbox claim %s/%s during rollback; local lease %s retained for retry with %s %s: %w",
			b.cfg.AgentSandbox.Namespace, claimName, leaseID, stopCommand, shellQuote(leaseID), cleanupErr),
	)
}

func (b *backend) persistAmbiguousClaimRecovery(
	leaseID, slug string,
	repo Repo,
	reclaim bool,
	claimName, expiresAt, recoveryNonce string,
	cause error,
) error {
	pending := sandboxReadiness{ClaimName: claimName}
	if _, err := writeClaimLease(b.cfg, leaseID, slug, repo, reclaim, pending, claimName, expiresAt, recoveryNonce); err != nil {
		return errors.Join(
			cause,
			fmt.Errorf("failed to persist recovery lease %s for ambiguous agent-sandbox claim %s/%s; manual cleanup may be required: %w",
				leaseID, b.cfg.AgentSandbox.Namespace, claimName, err),
		)
	}
	stopCommand := agentSandboxRecoveryCommand(b.cfg, "stop")
	return errors.Join(
		cause,
		fmt.Errorf("ambiguous agent-sandbox claim %s/%s may exist; local lease %s retained for exact-identity recovery with %s %s",
			b.cfg.AgentSandbox.Namespace, claimName, leaseID, stopCommand, shellQuote(leaseID)),
	)
}

func (b *backend) recoverAmbiguousCreateFailure(
	leaseID, slug string,
	repo Repo,
	reclaim bool,
	claimName, expiresAt, recoveryNonce string,
	cause error,
) error {
	var unresolved ambiguousClaimRecoveryUnknownError
	if !errors.As(cause, &unresolved) {
		return cause
	}
	return b.persistAmbiguousClaimRecovery(leaseID, slug, repo, reclaim, claimName, expiresAt, recoveryNonce, cause)
}

func (b *backend) claimIdentityForLiveClaim(claim LeaseClaim, live *kubernetesObject, persist bool) (LeaseClaim, claimIdentity, error) {
	if uid := strings.TrimSpace(claim.Labels[claimLabelClaimUID]); uid != "" {
		identity, err := claimIdentityFromLocalClaim(claim)
		if err != nil {
			return LeaseClaim{}, claimIdentity{}, err
		}
		if identity.ProviderScope == "" {
			identity.ProviderScope = claimScope(b.cfg)
		}
		if err := validateClaimIdentity(live, identity); err != nil {
			return LeaseClaim{}, claimIdentity{}, err
		}
		if err := validateClaimRecoveryNonce(live, strings.TrimSpace(claim.Labels[claimLabelRecoveryNonce])); err != nil {
			return LeaseClaim{}, claimIdentity{}, err
		}
		return claim, identity, nil
	}
	if !strings.EqualFold(strings.TrimSpace(claim.Labels[claimLabelClaimUIDPending]), "true") {
		return LeaseClaim{}, claimIdentity{}, exit(4, "agent-sandbox lease %s has no pinned Kubernetes claim UID", claim.LeaseID)
	}
	recoveryNonce := strings.TrimSpace(claim.Labels[claimLabelRecoveryNonce])
	if recoveryNonce == "" {
		return LeaseClaim{}, claimIdentity{}, exit(4, "agent-sandbox recovery lease %s has no pinned recovery nonce", claim.LeaseID)
	}
	expectedName := claimName(claim.LeaseID, claim.Slug)
	if got := strings.TrimSpace(claimNameFromLocalClaim(claim)); got != expectedName {
		return LeaseClaim{}, claimIdentity{}, exit(4, "agent-sandbox recovery lease %s claim name changed from %s to %s", claim.LeaseID, expectedName, blank(got, "<empty>"))
	}
	if live == nil || strings.TrimSpace(live.Metadata.Name) != expectedName {
		got := "<empty>"
		if live != nil {
			got = blank(strings.TrimSpace(live.Metadata.Name), "<empty>")
		}
		return LeaseClaim{}, claimIdentity{}, exit(4, "agent-sandbox recovery lease %s expected claim %s, got %s", claim.LeaseID, expectedName, got)
	}
	uid := strings.TrimSpace(live.Metadata.UID)
	if uid == "" {
		return LeaseClaim{}, claimIdentity{}, exit(4, "agent-sandbox recovery claim %s has no Kubernetes UID", expectedName)
	}
	identity, err := claimIdentityFromLocalClaimWithUID(claim, uid)
	if err != nil {
		return LeaseClaim{}, claimIdentity{}, err
	}
	if identity.ProviderScope == "" {
		identity.ProviderScope = claimScope(b.cfg)
	}
	if err := validateClaimIdentity(live, identity); err != nil {
		return LeaseClaim{}, claimIdentity{}, fmt.Errorf("refuse ambiguous agent-sandbox claim recovery: %w", err)
	}
	if err := validateClaimRecoveryNonce(live, recoveryNonce); err != nil {
		return LeaseClaim{}, claimIdentity{}, fmt.Errorf("refuse ambiguous agent-sandbox claim recovery: %w", err)
	}
	if !persist {
		return claim, identity, nil
	}
	labels := cloneStringMap(claim.Labels)
	labels[claimLabelClaimUID] = uid
	labels[claimLabelClaimUIDPending] = "false"
	updated, err := updateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels)
	if err != nil {
		return LeaseClaim{}, claimIdentity{}, fmt.Errorf("pin recovered agent-sandbox claim %s UID: %w", expectedName, err)
	}
	return updated, identity, nil
}

func (b *backend) deleteOwnedClaim(ctx context.Context, client kubernetesClient, claim LeaseClaim, leaseID, claimName string, forgetMissing bool) error {
	live, err := client.Get(ctx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName)
	if err != nil {
		if isNotFound(err) {
			if forgetMissing && b.cfg.AgentSandbox.ForgetMissing {
				fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing agent-sandbox claim=%s after explicit request\n", claimName)
				return b.removeLocalClaim(leaseID, claim)
			}
			if claim.LeaseID != "" {
				return retainMissingClaim(b.cfg, claim)
			}
		}
		return err
	}
	claim, identity, err := b.claimIdentityForLiveClaim(claim, live, true)
	if err != nil {
		return err
	}
	if err := client.Delete(ctx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName, identity.UID); err != nil && !isNotFound(err) {
		return err
	}
	if err := b.removeLocalClaim(leaseID, claim); err != nil {
		return fmt.Errorf("agent-sandbox claim %s/%s deleted but local lease %s removal failed: %w", b.cfg.AgentSandbox.Namespace, claimName, leaseID, err)
	}
	return nil
}

func validateClaimOwnership(obj *kubernetesObject, leaseID, providerScope string) error {
	labels := obj.Metadata.Labels
	if labels[labelProvider] != providerName || labels[labelLeaseID] != safeLabelValue(leaseID) {
		return exit(4, "agent-sandbox SandboxClaim %s is not owned by Crabbox lease %s", obj.Metadata.Name, leaseID)
	}
	scope := strings.TrimSpace(obj.Metadata.Annotations[annotationScope])
	if scope == "" {
		return exit(4, "agent-sandbox SandboxClaim %s has no Crabbox scope annotation", obj.Metadata.Name)
	}
	if providerScope != "" && scope != scopeFingerprint(providerScope) {
		return exit(4, "agent-sandbox SandboxClaim %s belongs to a different Crabbox scope", obj.Metadata.Name)
	}
	return nil
}

func validateClaimIdentity(obj *kubernetesObject, identity claimIdentity) error {
	if obj == nil {
		return resourceIdentityError{err: exit(4, "agent-sandbox claim identity is missing")}
	}
	if identity.UID == "" {
		return resourceIdentityError{err: exit(4, "agent-sandbox lease %s has no pinned Kubernetes claim UID", identity.LeaseID)}
	}
	if identity.WarmPool == "" {
		return resourceIdentityError{err: exit(4, "agent-sandbox lease %s has no pinned SandboxWarmPool", identity.LeaseID)}
	}
	if got := strings.TrimSpace(obj.Metadata.UID); got != identity.UID {
		return resourceIdentityError{err: exit(4, "agent-sandbox SandboxClaim %s UID changed from %s to %s", obj.Metadata.Name, identity.UID, blank(got, "<empty>"))}
	}
	if got := sandboxClaimWarmPool(obj); got != identity.WarmPool {
		return resourceIdentityError{err: exit(4, "agent-sandbox SandboxClaim %s warm pool changed from %s to %s", obj.Metadata.Name, identity.WarmPool, blank(got, "<empty>"))}
	}
	if identity.ExpiresAt != "" {
		shutdownTime, shutdownPolicy := sandboxClaimLifecycle(obj)
		if shutdownTime != identity.ExpiresAt || shutdownPolicy != "Retain" {
			return resourceIdentityError{err: exit(4, "agent-sandbox SandboxClaim %s lifecycle changed from shutdownTime=%s shutdownPolicy=Retain to shutdownTime=%s shutdownPolicy=%s", obj.Metadata.Name, identity.ExpiresAt, blank(shutdownTime, "<empty>"), blank(shutdownPolicy, "<empty>"))}
		}
	}
	if err := validateClaimOwnership(obj, identity.LeaseID, identity.ProviderScope); err != nil {
		return resourceIdentityError{err: err}
	}
	return nil
}

func validateClaimRecoveryNonce(obj *kubernetesObject, expected string) error {
	if expected == "" {
		return nil
	}
	got := ""
	if obj != nil {
		got = strings.TrimSpace(obj.Metadata.Annotations[annotationRecovery])
	}
	if got != expected {
		name := "<missing>"
		if obj != nil {
			name = blank(strings.TrimSpace(obj.Metadata.Name), "<missing>")
		}
		return resourceIdentityError{err: exit(4, "agent-sandbox SandboxClaim %s recovery nonce changed", name)}
	}
	return nil
}

func sandboxClaimWarmPool(obj *kubernetesObject) string {
	if obj == nil {
		return ""
	}
	ref, _ := obj.Spec["warmPoolRef"].(map[string]any)
	name, _ := ref["name"].(string)
	return strings.TrimSpace(name)
}

func sandboxClaimLifecycle(obj *kubernetesObject) (string, string) {
	if obj == nil {
		return "", ""
	}
	lifecycle, _ := obj.Spec["lifecycle"].(map[string]any)
	shutdownTime, _ := lifecycle["shutdownTime"].(string)
	shutdownPolicy, _ := lifecycle["shutdownPolicy"].(string)
	return strings.TrimSpace(shutdownTime), strings.TrimSpace(shutdownPolicy)
}

func sandboxClaimExpired(claim LeaseClaim, obj *kubernetesObject, now time.Time) (bool, string) {
	if claimTTLExpired(claim, now) {
		return true, "TTL expired at " + strings.TrimSpace(claim.Labels[claimLabelExpiresAt])
	}
	if obj != nil {
		if reason, expired := sandboxClaimControllerExpiry(obj); expired {
			return true, "controller reported " + reason
		}
	}
	return false, ""
}

func sandboxClaimControllerExpiry(obj *kubernetesObject) (string, bool) {
	if obj == nil {
		return "", false
	}
	for _, condition := range obj.Status.Conditions {
		reason := strings.TrimSpace(condition.Reason)
		if strings.EqualFold(reason, "ClaimExpired") || strings.EqualFold(reason, "SandboxExpired") {
			return reason, true
		}
	}
	return "", false
}

func (b *backend) missingClaimRunError(claim LeaseClaim) error {
	if b.cfg.AgentSandbox.ForgetMissing {
		if err := b.removeLocalClaim(claim.LeaseID, claim); err != nil {
			return errors.Join(
				exit(4, "agent-sandbox claim %s is missing in Kubernetes; command not run", claim.LeaseID),
				fmt.Errorf("remove local agent-sandbox lease %s: %w", claim.LeaseID, err),
			)
		}
		return exit(4, "agent-sandbox claim %s is missing in Kubernetes; local claim forgotten, command not run", claim.LeaseID)
	}
	return retainMissingClaim(b.cfg, claim)
}

func (b *backend) readinessRunError(ctx context.Context, client kubernetesClient, claim LeaseClaim, claimName string, readinessErr error) error {
	if !isNotFound(readinessErr) {
		return readinessErr
	}
	_, err := client.Get(ctx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName)
	if err == nil {
		return readinessErr
	}
	if isNotFound(err) {
		return b.missingClaimRunError(claim)
	}
	return errors.Join(readinessErr, fmt.Errorf("recheck agent-sandbox claim %s/%s after readiness failure: %w", b.cfg.AgentSandbox.Namespace, claimName, err))
}

func (b *backend) removeLocalClaim(leaseID string, claim LeaseClaim) error {
	if b.removeClaim != nil {
		return b.removeClaim(leaseID, claim)
	}
	return removeLeaseClaimIfUnchanged(leaseID, claim)
}

func (b *backend) client(ctx context.Context) (kubernetesClient, error) {
	if b.newClient != nil {
		return b.newClient(ctx, b.cfg, b.rt)
	}
	return newKubernetesClient(ctx, b.cfg, b.rt)
}

func (b *backend) doctorChecks(ctx context.Context, client kubernetesClient) ([]DoctorCheck, error) {
	cfg := b.cfg.AgentSandbox
	checks := []DoctorCheck{}
	add := func(status, check, message string, details map[string]string) {
		checks = append(checks, DoctorCheck{Status: status, Check: check, Message: message, Details: details})
	}
	if err := client.CheckResource(ctx, agentSandboxCoreGroupVersion, sandboxResource); err != nil {
		add("blocked", "crd.sandboxes", err.Error(), nil)
		return checks, err
	}
	add("ok", "crd.sandboxes", "found", map[string]string{"groupVersion": agentSandboxCoreGroupVersion})
	for _, resource := range []string{sandboxClaimResource, warmPoolResource} {
		if err := client.CheckResource(ctx, agentSandboxExtensionsGroupVersion, resource); err != nil {
			add("blocked", "crd."+resource, err.Error(), nil)
			return checks, err
		}
		add("ok", "crd."+resource, "found", map[string]string{"groupVersion": agentSandboxExtensionsGroupVersion})
	}
	if _, err := client.Get(ctx, warmPoolGVR(), cfg.Namespace, cfg.WarmPool); err != nil {
		add("blocked", "warm_pool", err.Error(), map[string]string{"namespace": cfg.Namespace, "name": cfg.WarmPool})
		return checks, err
	}
	add("ok", "warm_pool", "found", map[string]string{"namespace": cfg.Namespace, "name": cfg.WarmPool})
	for _, rule := range doctorRBACRules(cfg.Namespace) {
		allowed, err := client.CanI(ctx, rule)
		if err != nil {
			add("blocked", "rbac."+rule.String(), err.Error(), nil)
			return checks, err
		}
		if !allowed {
			err := exit(5, "agent-sandbox RBAC denied: %s", rule.String())
			add("blocked", "rbac."+rule.String(), err.Error(), nil)
			return checks, err
		}
		add("ok", "rbac."+rule.String(), "allowed", nil)
	}
	return checks, nil
}

func doctorRBACRules(namespace string) []rbacRule {
	return []rbacRule{
		{Group: "extensions.agents.x-k8s.io", Resource: sandboxClaimResource, Namespace: namespace, Verbs: []string{"get", "create", "delete"}},
		{Group: "extensions.agents.x-k8s.io", Resource: warmPoolResource, Namespace: namespace, Verbs: []string{"get"}},
		{Group: "agents.x-k8s.io", Resource: sandboxResource, Namespace: namespace, Verbs: []string{"get"}},
		{Group: "", Resource: podResource, Namespace: namespace, Verbs: []string{"get", "list"}},
		{Group: "", Resource: podResource, Subresource: "exec", Namespace: namespace, Verbs: []string{"create"}},
	}
}

type rbacRule struct {
	Group       string
	Resource    string
	Subresource string
	Namespace   string
	Verbs       []string
}

func (r rbacRule) String() string {
	group := r.Group
	if group == "" {
		group = "core"
	}
	resource := r.Resource
	if r.Subresource != "" {
		resource += "/" + r.Subresource
	}
	return strings.Join(r.Verbs, ",") + " " + group + "/" + resource + " namespace=" + r.Namespace
}
