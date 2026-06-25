package cua

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const cuaCleanupTimeout = 15 * time.Second

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b backend) Spec() ProviderSpec { return b.spec }

func (b backend) client() *bridgeClient {
	return newBridgeClient(b.cfg, b.rt)
}

func (b backend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	started := b.now()
	leaseID, sandboxID, slug, err := b.createSandbox(ctx, b.client(), req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	workdir, err := cuaWorkdir(b.cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s workdir=%s\n", leaseID, slug, providerName, sandboxID, workdir)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: cua warmup keeps the sandbox until explicit stop\n")
	}
	total := b.now().Sub(started)
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", total.Round(time.Millisecond))
	if req.TimingJSON {
		return writeTimingJSON(b.rt.Stderr, timingReport{
			Provider: providerName,
			LeaseID:  leaseID,
			Slug:     slug,
			TotalMs:  total.Milliseconds(),
			ExitCode: 0,
		})
	}
	return nil
}

func (b backend) Run(ctx context.Context, req RunRequest) (result RunResult, retErr error) {
	if req.Options.Tailscale.Enabled {
		return RunResult{}, exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	var command []string
	if !req.SyncOnly {
		var err error
		command, err = buildCommand(req.Command, req.ShellMode)
		if err != nil {
			return RunResult{}, err
		}
	}
	workdir, err := cuaWorkdir(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	client := b.client()
	leaseID, sandboxID, slug := "", "", ""
	acquired := false
	var operationUnlock func()
	if req.ID == "" {
		leaseID = newCUALeaseID()
		operationUnlock, err = lockCUALeaseOperation(ctx, leaseID)
		if err != nil {
			return RunResult{}, err
		}
		defer operationUnlock()
		operationUnlock = nil
		leaseID, sandboxID, slug, err = b.createSandboxWithLease(ctx, client, req.Repo, req.Reclaim, req.RequestedSlug, leaseID)
		if err != nil {
			return RunResult{}, err
		}
		acquired = true
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandboxID)
	} else {
		claim, ok, err := resolveClaim(req.ID, b.cfg)
		if err != nil {
			return RunResult{}, err
		}
		if !ok {
			return RunResult{}, exit(4, "CUA sandbox %q is not claimed by Crabbox", req.ID)
		}
		leaseID, sandboxID, slug = claim.LeaseID, claimSandboxName(claim), blank(claim.Slug, newLeaseSlug(claim.LeaseID))
		if claimCleanupInProgress(claim) {
			return RunResult{}, exit(4, "CUA lease=%s cleanup is already in progress", claim.LeaseID)
		}
		unlock, err := lockCUALeaseOperation(ctx, claim.LeaseID)
		if err != nil {
			return RunResult{}, err
		}
		operationUnlock = unlock
		defer operationUnlock()
		operationUnlock = nil
		if _, err := b.verifyClaim(ctx, client, claim); err != nil {
			return RunResult{}, err
		}
		if err := b.refreshLeaseActivity(claim, req.Repo.Root, req.Reclaim); err != nil {
			return RunResult{}, fmt.Errorf("refresh CUA lease activity before reuse: %w", err)
		}
	}
	if operationUnlock != nil {
		defer operationUnlock()
	}

	shouldStop := acquired && !req.Keep
	if shouldStop {
		defer func() {
			if cleanupErr := b.cleanupCreatedRun(ctx, client, leaseID, sandboxID, &shouldStop); cleanupErr != nil {
				if result.ExitCode == 0 {
					result.ExitCode = 1
				}
				retErr = errors.Join(retErr, cleanupErr)
			}
		}()
	}
	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s\n", providerName, leaseID, sandboxID, workdir)

	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, client, sandboxID, req, workdir)
		if err != nil {
			handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
			return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.ensureWorkspace(ctx, client, sandboxID, workdir); err != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true}, err
	}

	if req.SyncOnly {
		result = RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workdir)
		activityErr := b.refreshLeaseActivityIfRetained(leaseID, shouldStop)
		if cleanupErr := b.cleanupCreatedRun(ctx, client, leaseID, sandboxID, &shouldStop); cleanupErr != nil {
			result.ExitCode = 1
			return result, cleanupErr
		}
		if req.TimingJSON {
			if err := writeTimingJSON(b.rt.Stderr, timingReport{
				Provider:      providerName,
				LeaseID:       leaseID,
				Slug:          slug,
				SyncDelegated: true,
				SyncMs:        syncDuration.Milliseconds(),
				SyncPhases:    syncPhases,
				SyncSkipped:   req.NoSync,
				TotalMs:       result.Total.Milliseconds(),
				ExitCode:      result.ExitCode,
				Label:         strings.TrimSpace(req.Label),
			}); err != nil {
				return result, err
			}
		}
		return result, activityErr
	}

	commandEnv, stripped := commandEnv(req.Env)
	if len(stripped) > 0 {
		fmt.Fprintf(b.rt.Stderr, "warning: provider=%s did not forward provider authentication variables: %s\n", providerName, strings.Join(stripped, ","))
	}
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, commandEnv)
	}
	commandStart := b.now()
	var stdout, stderr bytes.Buffer
	resp, runErr := client.Exec(ctx, sandboxID, command, workdir, commandEnv, &stdout, &stderr)
	_, _ = io.Copy(b.rt.Stdout, &stdout)
	_, _ = io.Copy(b.rt.Stderr, &stderr)
	commandDuration := b.now().Sub(commandStart)
	result = RunResult{
		Provider:      providerName,
		LeaseID:       leaseID,
		Slug:          slug,
		CommandText:   shellScriptFromArgv(command),
		ExitCode:      resp.ExitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
	}
	if runErr != nil && result.ExitCode == 0 {
		result.ExitCode = 1
	}
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "cua run summary sync_skipped=true command=%s total=%s exit=%d\n", result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "cua run summary sync=%s command=%s total=%s exit=%d\n", syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	}
	var commandErr error
	if runErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		commandErr = ExitError{Code: result.ExitCode, Message: fmt.Sprintf("cua run failed: %v", redactSecrets(runErr.Error()))}
	} else if result.ExitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		commandErr = ExitError{Code: result.ExitCode, Message: fmt.Sprintf("cua run exited %d", result.ExitCode)}
	}
	if activityErr := b.refreshLeaseActivityIfRetained(leaseID, shouldStop); activityErr != nil && commandErr == nil {
		result.ExitCode = 1
		commandErr = activityErr
	}
	if cleanupErr := b.cleanupCreatedRun(ctx, client, leaseID, sandboxID, &shouldStop); cleanupErr != nil {
		if result.ExitCode == 0 {
			result.ExitCode = 1
		}
		commandErr = errors.Join(commandErr, cleanupErr)
	}
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
			Provider:      providerName,
			LeaseID:       leaseID,
			Slug:          slug,
			SyncDelegated: true,
			SyncMs:        syncDuration.Milliseconds(),
			SyncPhases:    syncPhases,
			SyncSkipped:   req.NoSync,
			CommandMs:     result.Command.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      result.ExitCode,
			Label:         strings.TrimSpace(req.Label),
		}, result, commandErr)); err != nil {
			return result, err
		}
	}
	return result, commandErr
}

func (b backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	client := b.client()
	claims, err := listCUALeaseClaims()
	if err != nil {
		return nil, err
	}
	views := make([]LeaseView, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider != providerName || !b.claimMatchesActiveScope(claim) {
			continue
		}
		sandboxName := claimSandboxName(claim)
		if sandboxName == "" {
			continue
		}
		sb, getErr := client.GetSandbox(ctx, sandboxName)
		state := ""
		if getErr != nil {
			if !isCUANotFound(getErr) {
				return nil, getErr
			}
			state = "missing-or-inaccessible"
		} else {
			if err := validateSandboxOwnership(claim, sb, claim.ProviderScope); err != nil {
				return nil, err
			}
			state = normalizedSandboxState(sb)
		}
		views = append(views, b.serverFromSandbox(claim, bridgeSandboxSummary{ID: sandboxName, Name: sandboxName, Status: state}))
	}
	return views, nil
}

func (b backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	claim, ok, err := resolveClaim(req.ID, b.cfg)
	if err != nil {
		return StatusView{}, err
	}
	if !ok {
		return StatusView{}, exit(4, "CUA sandbox %q is not claimed by Crabbox", req.ID)
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
	deadline := b.now().Add(waitTimeout)
	for {
		sb, getErr := b.verifyClaim(pollCtx, b.client(), claim)
		if getErr != nil {
			if req.Wait && ctx.Err() == nil && pollCtx.Err() != nil {
				return StatusView{}, exit(5, "timed out waiting for CUA sandbox %s to become ready", claimSandboxName(claim))
			}
			return StatusView{}, getErr
		}
		state := normalizedSandboxState(sb)
		view := StatusView{
			ID:       claim.LeaseID,
			Slug:     blank(claim.Slug, newLeaseSlug(claim.LeaseID)),
			Provider: providerName,
			TargetOS: targetLinux,
			State:    state,
			ServerID: claimSandboxName(claim),
			Pond:     claim.Pond,
			Network:  "public",
			Ready:    isReadyState(state),
			Labels: map[string]string{
				"provider": providerName,
				"lease":    claim.LeaseID,
				"slug":     blank(claim.Slug, newLeaseSlug(claim.LeaseID)),
				"pond":     claim.Pond,
				"state":    state,
			},
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if isTerminalState(state) {
			return StatusView{}, exit(5, "CUA sandbox %s entered terminal state %q before becoming ready", claimSandboxName(claim), state)
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for CUA sandbox %s to become ready", claimSandboxName(claim))
		}
		select {
		case <-pollCtx.Done():
			if ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for CUA sandbox %s to become ready", claimSandboxName(claim))
			}
			return StatusView{}, pollCtx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b backend) Stop(ctx context.Context, req StopRequest) error {
	claim, ok, err := resolveClaim(req.ID, b.cfg)
	if err != nil {
		return err
	}
	if !ok {
		return exit(4, "CUA sandbox %q is not claimed by Crabbox", req.ID)
	}
	unlock, err := lockCUALeaseOperation(ctx, claim.LeaseID)
	if err != nil {
		return err
	}
	defer unlock()
	claim, err = readLeaseClaim(claim.LeaseID)
	if err != nil {
		return err
	}
	if claim.LeaseID == "" {
		return exit(4, "CUA sandbox %q is not claimed by Crabbox", req.ID)
	}
	client := b.client()
	if _, err := b.verifyClaim(ctx, client, claim); err != nil {
		if !isCUANotFound(err) || !b.cfg.Cua.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing CUA sandbox=%s after explicit request\n", claimSandboxName(claim))
		removeLeaseClaim(claim.LeaseID)
		return nil
	}
	transitioned, err := markCleanupIfUnchanged(claim)
	if err != nil {
		return fmt.Errorf("mark CUA lease=%s cleanup before stop: %w", claim.LeaseID, err)
	}
	if err := client.DeleteSandbox(ctx, claimSandboxName(claim)); err != nil {
		if !isCUANotFound(err) || !b.cfg.Cua.ForgetMissing {
			if restoreErr := restoreCleanupMark(transitioned, claim); restoreErr != nil {
				return errors.Join(err, restoreErr)
			}
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing CUA sandbox=%s after explicit request\n", claimSandboxName(claim))
	}
	if err := removeLeaseClaimIfUnchanged(claim.LeaseID, transitioned); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", claim.LeaseID, claimSandboxName(claim))
	return nil
}

func (b backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	claims, err := listCUALeaseClaims()
	if err != nil {
		return err
	}
	client := b.client()
	now := b.now().UTC()
	checked := 0
	removed := 0
	claimsRemoved := 0
	for _, listed := range claims {
		if listed.Provider != providerName || !b.claimMatchesActiveScope(listed) {
			continue
		}
		unlock, err := lockCUALeaseOperation(ctx, listed.LeaseID)
		if err != nil {
			return err
		}
		err = func() error {
			defer unlock()
			claim, err := readLeaseClaim(listed.LeaseID)
			if err != nil {
				return err
			}
			if claim.LeaseID == "" || claim.Provider != providerName || !b.claimMatchesActiveScope(claim) {
				return nil
			}
			checked++
			sandboxName := claimSandboxName(claim)
			sb, getErr := client.GetSandbox(ctx, sandboxName)
			if getErr != nil {
				if !isCUANotFound(getErr) {
					return getErr
				}
				markedCleanup := claimCleanupInProgress(claim)
				if !markedCleanup && !b.cfg.Cua.ForgetMissing {
					fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=missing-or-inaccessible; set cua.forgetMissing to remove the claim\n", sandboxName, claim.LeaseID)
					return nil
				}
				reason := "missing sandbox"
				if markedCleanup {
					reason = "cleanup marker missing sandbox"
				}
				if req.DryRun {
					fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s slug=%s reason=%s\n", claim.LeaseID, blank(claim.Slug, "-"), reason)
					return nil
				}
				if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
					return err
				}
				fmt.Fprintf(b.rt.Stdout, "remove claim lease=%s slug=%s reason=%s\n", claim.LeaseID, blank(claim.Slug, "-"), reason)
				claimsRemoved++
				return nil
			}
			if err := validateSandboxOwnership(claim, sb, claim.ProviderScope); err != nil {
				return err
			}
			due, reason := claimCleanupDue(claim, now)
			if !due {
				fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=%s\n", sandboxName, claim.LeaseID, reason)
				return nil
			}
			if req.DryRun {
				fmt.Fprintf(b.rt.Stdout, "would delete sandbox=%s lease=%s reason=%s\n", sandboxName, claim.LeaseID, reason)
				return nil
			}
			transitioned, err := markCleanupIfUnchanged(claim)
			if err != nil {
				fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=claim changed during cleanup\n", sandboxName, claim.LeaseID)
				return nil
			}
			if err := client.DeleteSandbox(ctx, sandboxName); err != nil && !isCUANotFound(err) {
				if restoreErr := restoreCleanupMark(transitioned, claim); restoreErr != nil {
					return errors.Join(err, restoreErr)
				}
				return err
			}
			if err := removeLeaseClaimIfUnchanged(claim.LeaseID, transitioned); err != nil {
				return err
			}
			fmt.Fprintf(b.rt.Stdout, "delete sandbox=%s lease=%s reason=%s\n", sandboxName, claim.LeaseID, reason)
			removed++
			return nil
		}()
		if err != nil {
			return err
		}
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d claims_removed=%d checked=%d\n", providerName, removed, claimsRemoved, checked)
	}
	return nil
}

func (b backend) createSandbox(ctx context.Context, client *bridgeClient, repo Repo, reclaim bool, requestedSlug string) (string, string, string, error) {
	return b.createSandboxWithLease(ctx, client, repo, reclaim, requestedSlug, newCUALeaseID())
}

func (b backend) createSandboxWithLease(ctx context.Context, client *bridgeClient, repo Repo, reclaim bool, requestedSlug, leaseID string) (string, string, string, error) {
	if err := validateProviderConfig(b.cfg); err != nil {
		return "", "", "", err
	}
	scope, err := cuaScope(b.cfg)
	if err != nil {
		return "", "", "", err
	}
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", "", "", err
	}
	sandboxName := newSandboxName(leaseID, slug)
	sb, err := client.CreateSandbox(ctx, sandboxName, map[string]string{
		"crabbox.provider": providerName,
		"crabbox.scope":    scope,
		"crabbox.lease":    leaseID,
		"crabbox.slug":     slug,
	})
	if err != nil {
		return "", "", "", err
	}
	if sb.ID == "" {
		sb.ID = sandboxName
	}
	if sb.Name == "" {
		sb.Name = sandboxName
	}
	if err := validateSandboxOwnership(LeaseClaim{LeaseID: leaseID, Provider: providerName, ProviderScope: scope, CloudID: sandboxName}, sb, scope); err != nil {
		return "", "", "", b.cleanupCreateFailure(ctx, client, sandboxName, err)
	}
	if err := claimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, scope, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim, b.serverFromSandbox(LeaseClaim{LeaseID: leaseID, Slug: slug, Pond: b.cfg.Pond}, sb)); err != nil {
		return "", "", "", b.cleanupCreateFailure(ctx, client, sandboxName, err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return "", "", "", b.cleanupCreateFailure(ctx, client, sandboxName, err)
	}
	if _, err := updateLeaseClaimLabelsIfUnchanged(leaseID, claim, claimLabels(b.cfg, sandboxName, false)); err != nil {
		return "", "", "", b.cleanupCreateFailure(ctx, client, sandboxName, err)
	}
	return leaseID, sandboxName, slug, nil
}

func (b backend) verifyClaim(ctx context.Context, client *bridgeClient, claim LeaseClaim) (bridgeSandboxSummary, error) {
	scope, err := cuaScope(b.cfg)
	if err != nil {
		return bridgeSandboxSummary{}, err
	}
	if err := validateClaimScope(claim, scope); err != nil {
		return bridgeSandboxSummary{}, err
	}
	sandboxName := claimSandboxName(claim)
	if sandboxName == "" {
		return bridgeSandboxSummary{}, exit(4, "CUA lease %q is missing its claimed sandbox name", claim.LeaseID)
	}
	sb, err := client.GetSandbox(ctx, sandboxName)
	if err != nil {
		return bridgeSandboxSummary{}, err
	}
	if err := validateSandboxOwnership(claim, sb, scope); err != nil {
		return bridgeSandboxSummary{}, err
	}
	return sb, nil
}

func (b backend) serverFromSandbox(claim LeaseClaim, sb bridgeSandboxSummary) Server {
	state := normalizedSandboxState(sb)
	sandboxName := strings.TrimSpace(blank(sb.Name, sb.ID))
	if sandboxName == "" {
		sandboxName = claimSandboxName(claim)
	}
	return Server{
		Provider: providerName,
		CloudID:  sandboxName,
		Name:     sandboxName,
		Status:   state,
		Labels: map[string]string{
			"provider": providerName,
			"lease":    claim.LeaseID,
			"slug":     claim.Slug,
			"pond":     claim.Pond,
			"target":   targetLinux,
			"state":    state,
		},
	}
}

func (b backend) claimMatchesActiveScope(claim LeaseClaim) bool {
	scope, err := cuaScope(b.cfg)
	return err == nil && claim.ProviderScope == scope
}

func (b backend) refreshLeaseActivity(claim LeaseClaim, repoRoot string, reclaim bool) error {
	if claim.LeaseID == "" {
		return nil
	}
	if repoRoot == "" {
		repoRoot = claim.RepoRoot
	}
	timeout := b.cfg.IdleTimeout
	if timeout <= 0 && claim.IdleTimeoutSeconds > 0 {
		timeout = time.Duration(claim.IdleTimeoutSeconds) * time.Second
	}
	if claimCleanupInProgress(claim) {
		return exit(4, "CUA lease=%s cleanup is already in progress", claim.LeaseID)
	}
	if _, err := claimLeaseForRepoProviderScopePondIfUnchanged(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, claim.Pond, repoRoot, timeout, reclaim, claim, true); err != nil {
		return err
	}
	updated, err := readLeaseClaim(claim.LeaseID)
	if err != nil {
		return err
	}
	_, err = updateLeaseClaimLabelsIfUnchanged(claim.LeaseID, updated, claimLabels(b.cfg, claimSandboxName(claim), claimIsMissing(claim)))
	return err
}

func (b backend) refreshLeaseActivityIfRetained(leaseID string, shouldStop bool) error {
	if shouldStop || leaseID == "" {
		return nil
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return err
	}
	return b.refreshLeaseActivity(claim, claim.RepoRoot, false)
}

func markCleanupIfUnchanged(claim LeaseClaim) (LeaseClaim, error) {
	if claimCleanupInProgress(claim) {
		return claim, nil
	}
	labels := make(map[string]string, len(claim.Labels)+1)
	for key, value := range claim.Labels {
		labels[key] = value
	}
	labels[labelState] = "cleanup"
	return updateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels)
}

func restoreCleanupMark(current, previous LeaseClaim) error {
	if current.LeaseID == "" {
		return nil
	}
	return restoreLeaseClaimIfUnchanged(current.LeaseID, current, previous, true)
}

func (b backend) cleanupCreateFailure(ctx context.Context, client *bridgeClient, sandboxID string, cause error) error {
	if sandboxID == "" {
		return cause
	}
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := client.DeleteSandbox(cleanupCtx, sandboxID); err != nil && !isCUANotFound(err) {
		return errors.Join(cause, fmt.Errorf("CUA cleanup failed for sandbox %s; delete it in CUA manually: %w", sandboxID, err))
	}
	return cause
}

func (b backend) cleanupCreatedRun(ctx context.Context, client *bridgeClient, leaseID, sandboxID string, shouldStop *bool) error {
	if !*shouldStop {
		return nil
	}
	*shouldStop = false
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := client.DeleteSandbox(cleanupCtx, sandboxID); err != nil && !isCUANotFound(err) {
		return fmt.Errorf("CUA delete failed for %s: %w", sandboxID, err)
	}
	removeLeaseClaim(leaseID)
	return nil
}

func (b backend) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cuaCleanupTimeout)
}

func (b backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

func normalizedSandboxState(sb bridgeSandboxSummary) string {
	return strings.ToLower(blank(strings.TrimSpace(blank(sb.Status, sb.State)), "unknown"))
}

func isReadyState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "running", "ready", "started", "active":
		return true
	default:
		return false
	}
}

func isTerminalState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "terminated", "stopped", "failed", "error", "aborted", "killed", "deleted", "destroyed":
		return true
	default:
		return false
	}
}

func claimCleanupDue(claim LeaseClaim, now time.Time) (bool, string) {
	if claimCleanupInProgress(claim) {
		return true, "cleanup marker"
	}
	if claim.IdleTimeoutSeconds <= 0 {
		return false, "idle timeout disabled"
	}
	lastUsed, err := time.Parse(time.RFC3339, strings.TrimSpace(claim.LastUsedAt))
	if err != nil {
		return false, "invalid last-used time"
	}
	deadline := lastUsed.Add(time.Duration(claim.IdleTimeoutSeconds) * time.Second)
	if now.Before(deadline) {
		return false, "idle timeout not reached"
	}
	return true, "idle timeout"
}

func buildCommand(command []string, shellMode bool) ([]string, error) {
	if len(command) == 0 {
		return nil, errors.New("missing command")
	}
	if shellMode {
		return []string{"bash", "-lc", strings.Join(command, " ")}, nil
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		if len(command) == 1 {
			return []string{"bash", "-lc", command[0]}, nil
		}
		return []string{"bash", "-lc", shellScriptFromArgv(command)}, nil
	}
	return command, nil
}

func commandEnv(env map[string]string) (map[string]string, []string) {
	out := make(map[string]string, len(env))
	stripped := []string{}
	for key, value := range env {
		upper := strings.ToUpper(strings.TrimSpace(key))
		if upper == "CRABBOX_CUA_API_KEY" || upper == "CUA_API_KEY" || upper == "CUA_BASE_URL" || upper == "CRABBOX_CUA_API_URL" {
			stripped = append(stripped, key)
			continue
		}
		out[key] = value
	}
	return out, stripped
}

func bridgeFileFromReader(path string, body io.Reader) (bridgeFile, error) {
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, body); err != nil {
		return bridgeFile{}, err
	}
	return bridgeFile{Path: path, ContentBase64: base64.StdEncoding.EncodeToString(buf.Bytes())}, nil
}
