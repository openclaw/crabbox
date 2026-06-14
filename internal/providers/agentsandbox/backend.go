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
	spec      ProviderSpec
	cfg       Config
	rt        Runtime
	newClient func(context.Context, Config, Runtime) (kubernetesClient, error)
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
	leaseID, claimName, slug, ready, err := b.createClaim(ctx, client, req.RequestedSlug)
	if err != nil {
		return err
	}
	unlockOperation, err := lockAgentSandboxLeaseOperation(ctx, leaseID)
	if err != nil {
		cleanupCtx, cancel := b.cleanupContext(ctx)
		defer cancel()
		_ = client.Delete(cleanupCtx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName, ready.ClaimUID)
		return err
	}
	defer unlockOperation()
	if err := writeClaimLease(b.cfg, leaseID, slug, req.Repo, req.Reclaim, ready, claimName); err != nil {
		cleanupCtx, cancel := b.cleanupContext(ctx)
		defer cancel()
		_ = client.Delete(cleanupCtx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName, ready.ClaimUID)
		return err
	}
	total := b.now().Sub(started)
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s claim=%s sandbox=%s pod=%s\n", leaseID, slug, providerName, claimName, ready.SandboxName, ready.PodName)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: agent-sandbox warmup keeps the claim until explicit stop\n")
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
		leaseID, claimName, slug, ready, err = b.createClaim(ctx, client, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		unlockOperation, err = lockAgentSandboxLeaseOperation(ctx, leaseID)
		if err != nil {
			cleanupCtx, cancel := b.cleanupContext(ctx)
			defer cancel()
			_ = client.Delete(cleanupCtx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName, ready.ClaimUID)
			return RunResult{}, err
		}
		if err := writeClaimLease(b.cfg, leaseID, slug, req.Repo, req.Reclaim, ready, claimName); err != nil {
			cleanupCtx, cancel := b.cleanupContext(ctx)
			defer cancel()
			_ = client.Delete(cleanupCtx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName, ready.ClaimUID)
			return RunResult{}, err
		}
		claim, _ = readLeaseClaim(leaseID)
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
		identity, err := claimIdentityFromLocalClaim(claim)
		if err != nil {
			return RunResult{}, err
		}
		if err := claimLeaseForRepo(b.cfg, claim.LeaseID, claim.Slug, req.Repo, req.Reclaim); err != nil {
			return RunResult{}, err
		}
		updated, err := readLeaseClaim(claim.LeaseID)
		if err != nil {
			return RunResult{}, err
		}
		claim, err = updateLeaseClaimLabelsIfUnchanged(claim.LeaseID, updated, claim.Labels)
		if err != nil {
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
		if err := validateClaimIdentity(liveClaim, identity); err != nil {
			return RunResult{}, err
		}
		ready, err = waitForSandboxReadinessWithTimeouts(ctx, client, b.cfg.AgentSandbox.Namespace, claimName, identity, readinessTimeout(b.cfg), podReadinessTimeout(b.cfg), time.Second)
		if err != nil {
			if isNotFound(err) {
				return RunResult{}, b.missingClaimRunError(claim)
			}
			return RunResult{}, err
		}
	}
	shouldStop = acquired && !req.Keep && b.cfg.AgentSandbox.DeleteOnRelease
	var pendingTiming *timingReport
	defer func() {
		if shouldStop {
			cleanupCtx, cancel := b.cleanupContext(ctx)
			defer cancel()
			if cleanupErr := b.deleteOwnedClaim(cleanupCtx, client, claim, leaseID, claimName, false); cleanupErr != nil {
				if result.ExitCode == 0 {
					result.ExitCode = 1
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
			_ = writeTimingJSON(b.rt.Stderr, *pendingTiming)
		}
	}()
	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s claim=%s sandbox=%s pod=%s workdir=%s\n", providerName, leaseID, claimName, ready.SandboxName, ready.PodName, workdir)

	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, client, ready, req, workdir)
		if err != nil {
			handleDelegatedRunFailure(b.rt.Stderr, b.cfg, req, leaseID, slug, acquired, &shouldStop)
			return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.execShell(ctx, client, ready, "mkdir -p "+shellQuote(workdir)); err != nil {
		handleDelegatedRunFailure(b.rt.Stderr, b.cfg, req, leaseID, slug, acquired, &shouldStop)
		return RunResult{}, err
	}
	if req.SyncOnly {
		result = RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workdir)
		if !shouldStop {
			if err := refreshClaimLeaseActivity(b.cfg, claim); err != nil && claim.LeaseID != "" {
				fmt.Fprintf(b.rt.Stderr, "warning: refresh agent-sandbox lease activity failed lease=%s: %v\n", leaseID, err)
				result.ExitCode = 1
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
	if !shouldStop && commandErr == nil {
		if err := refreshClaimLeaseActivity(b.cfg, claim); err != nil && claim.LeaseID != "" {
			fmt.Fprintf(b.rt.Stderr, "warning: refresh agent-sandbox lease activity failed lease=%s: %v\n", leaseID, err)
			result.ExitCode = 1
			commandErr = err
		}
	}
	if req.TimingJSON {
		pendingTiming = &timingReport{Provider: providerName, LeaseID: leaseID, Slug: slug, SyncDelegated: true, SyncMs: syncDuration.Milliseconds(), SyncPhases: syncPhases, SyncSkipped: req.NoSync, CommandMs: commandDuration.Milliseconds(), TotalMs: result.Total.Milliseconds(), ExitCode: result.ExitCode, Label: strings.TrimSpace(req.Label)}
	}
	return result, commandErr
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
		identity, err := claimIdentityFromLocalClaim(claim)
		if err != nil {
			return nil, err
		}
		claimName := claimNameFromLocalClaim(claim)
		ready, err := sandboxReadinessOnce(ctx, client, b.cfg.AgentSandbox.Namespace, claimName, identity)
		state := statusViewReady
		if err != nil {
			if isNotFound(err) {
				state = "missing-or-inaccessible"
			} else if errors.Is(err, errNotReady) {
				state = "not-ready"
			} else {
				return nil, err
			}
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
	identity, err := claimIdentityFromLocalClaim(claim)
	if err != nil {
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
	liveClaim, err := client.Get(ctx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName)
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
	if err := validateClaimIdentity(liveClaim, identity); err != nil {
		return StatusView{}, err
	}
	for {
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
			_, getErr := client.Get(ctx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName)
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

func (b *backend) createClaim(ctx context.Context, client kubernetesClient, requestedSlug string) (string, string, string, sandboxReadiness, error) {
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", "", "", sandboxReadiness{}, err
	}
	claimName := claimName(leaseID, slug)
	obj := &kubernetesObject{
		APIVersion: agentSandboxExtensionsGroupVersion,
		Kind:       "SandboxClaim",
		Metadata: objectMeta{
			Name:        claimName,
			Namespace:   b.cfg.AgentSandbox.Namespace,
			Labels:      claimLabels(leaseID, slug),
			Annotations: claimAnnotations(b.cfg),
		},
		Spec: map[string]any{
			"warmPoolRef": map[string]any{"name": b.cfg.AgentSandbox.WarmPool},
		},
	}
	created, err := client.Create(ctx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, obj)
	if err != nil {
		return "", "", "", sandboxReadiness{}, err
	}
	identity := claimIdentity{LeaseID: leaseID, ProviderScope: claimScope(b.cfg), UID: strings.TrimSpace(created.Metadata.UID)}
	if identity.UID == "" {
		return "", "", "", sandboxReadiness{}, exit(4, "created agent-sandbox claim %s has no Kubernetes UID", claimName)
	}
	ready, err := waitForSandboxReadinessWithTimeouts(ctx, client, b.cfg.AgentSandbox.Namespace, claimName, identity, readinessTimeout(b.cfg), podReadinessTimeout(b.cfg), time.Second)
	if err != nil {
		cleanupCtx, cleanupCancel := b.cleanupContext(ctx)
		defer cleanupCancel()
		_ = client.Delete(cleanupCtx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName, identity.UID)
		return "", "", "", sandboxReadiness{}, err
	}
	return leaseID, claimName, slug, ready, nil
}

func (b *backend) deleteOwnedClaim(ctx context.Context, client kubernetesClient, claim LeaseClaim, leaseID, claimName string, forgetMissing bool) error {
	identity, err := claimIdentityFromLocalClaim(claim)
	if err != nil {
		return err
	}
	live, err := client.Get(ctx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName)
	if err != nil {
		if isNotFound(err) {
			if forgetMissing && b.cfg.AgentSandbox.ForgetMissing {
				fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing agent-sandbox claim=%s after explicit request\n", claimName)
				removeLeaseClaim(leaseID)
				return nil
			}
			if claim.LeaseID != "" {
				return retainMissingClaim(b.cfg, claim)
			}
		}
		return err
	}
	providerScope := claim.ProviderScope
	if providerScope == "" {
		providerScope = claimScope(b.cfg)
	}
	identity.LeaseID = leaseID
	identity.ProviderScope = providerScope
	if err := validateClaimIdentity(live, identity); err != nil {
		return err
	}
	if err := client.Delete(ctx, sandboxClaimGVR(), b.cfg.AgentSandbox.Namespace, claimName, identity.UID); err != nil && !isNotFound(err) {
		return err
	}
	removeLeaseClaim(leaseID)
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
		return exit(4, "agent-sandbox claim identity is missing")
	}
	if identity.UID == "" {
		return exit(4, "agent-sandbox lease %s has no pinned Kubernetes claim UID", identity.LeaseID)
	}
	if got := strings.TrimSpace(obj.Metadata.UID); got != identity.UID {
		return exit(4, "agent-sandbox SandboxClaim %s UID changed from %s to %s", obj.Metadata.Name, identity.UID, blank(got, "<empty>"))
	}
	return validateClaimOwnership(obj, identity.LeaseID, identity.ProviderScope)
}

func (b *backend) missingClaimRunError(claim LeaseClaim) error {
	if b.cfg.AgentSandbox.ForgetMissing {
		removeLeaseClaim(claim.LeaseID)
		return exit(4, "agent-sandbox claim %s is missing in Kubernetes; local claim forgotten, command not run", claim.LeaseID)
	}
	return retainMissingClaim(b.cfg, claim)
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
