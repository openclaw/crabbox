package vercelsandbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	vercelSandboxCleanupTimeout = 15 * time.Second

	metadataProviderKey = "crabbox.provider"
	metadataScopeKey    = "crabbox.scope"
	metadataClaimKey    = "crabbox.claim"
	metadataRepoKey     = "crabbox.repo"
	metadataSlugKey     = "crabbox.slug"
)

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	if _, err := vercelSandboxWorkdir(b.cfg); err != nil {
		return err
	}
	started := b.now()
	api, err := b.client()
	if err != nil {
		return err
	}
	if err := b.bindProviderScope(ctx, api, false); err != nil {
		return err
	}
	leaseID, sandboxID, slug, unlockOperation, err := b.createSandbox(ctx, api, req.Repo, req.Reclaim, req.RequestedSlug, true)
	if err != nil {
		return err
	}
	defer unlockOperation()
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s runtime=%s\n", leaseID, slug, providerName, sandboxID, vercelSandboxRuntime(b.cfg))
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: vercel-sandbox warmup keeps the sandbox until explicit stop\n")
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

func (b *backend) Run(ctx context.Context, req RunRequest) (result RunResult, retErr error) {
	if req.Options.Tailscale.Enabled {
		return RunResult{}, exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	workdir, err := vercelSandboxWorkdir(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	started := b.now()
	api, err := b.client()
	if err != nil {
		return RunResult{}, err
	}
	if err := b.bindProviderScope(ctx, api, req.ID != ""); err != nil {
		return RunResult{}, err
	}
	leaseID, sandboxID, slug := "", "", ""
	acquired := false
	var unlockOperation func()
	defer func() {
		if unlockOperation != nil {
			unlockOperation()
		}
	}()
	if req.ID == "" {
		leaseID, sandboxID, slug, unlockOperation, err = b.createSandbox(ctx, api, req.Repo, req.Reclaim, req.RequestedSlug, req.Keep || req.KeepOnFailure)
		if err != nil {
			return RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s runtime=%s\n", leaseID, slug, providerName, sandboxID, vercelSandboxRuntime(b.cfg))
		acquired = true
	} else {
		leaseID, sandboxID, _, err = b.resolveLeaseID(req.ID, "", false, 0)
		if err != nil {
			return RunResult{}, err
		}
		unlockOperation, err = lockVercelSandboxLeaseOperation(ctx, leaseID)
		if err != nil {
			return RunResult{}, err
		}
		leaseID, sandboxID, _, err = b.resolveLeaseID(leaseID, "", false, 0)
		if err != nil {
			return RunResult{}, err
		}
		if _, err := b.verifyClaim(ctx, api, leaseID, sandboxID); err != nil {
			return RunResult{}, err
		}
		claim, err := readLeaseClaim(leaseID)
		if err != nil {
			return RunResult{}, err
		}
		_, _, slug, err = b.finishResolvedLease(claim, req.Repo.Root, req.Reclaim, b.cfg.IdleTimeout)
		if err != nil {
			return RunResult{}, err
		}
	}
	shouldStop := acquired && !req.Keep
	if shouldStop {
		defer func() {
			if cleanupErr := b.cleanupCreatedRun(ctx, api, leaseID, sandboxID, &shouldStop); cleanupErr != nil {
				if result.ExitCode == 0 {
					result.ExitCode = 1
				}
				if retErr == nil {
					retErr = exit(1, "%v", cleanupErr)
				} else {
					retErr = errors.Join(retErr, cleanupErr)
				}
			}
		}()
	}
	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s\n", providerName, leaseID, sandboxID, workdir)

	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, api, sandboxID, req, workdir)
		if err != nil {
			handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
			return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.ensureWorkspace(ctx, api, sandboxID, workdir); err != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return RunResult{}, err
	}

	if req.SyncOnly {
		result = RunResult{
			Provider:      providerName,
			LeaseID:       leaseID,
			Slug:          slug,
			Total:         b.now().Sub(started),
			SyncDelegated: true,
		}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workdir)
		activityErr := b.refreshLeaseActivityIfRetained(leaseID, shouldStop)
		if activityErr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: refresh vercel-sandbox lease activity failed lease=%s: %v\n", leaseID, activityErr)
			result.ExitCode = 1
		}
		if cleanupErr := b.cleanupCreatedRun(ctx, api, leaseID, sandboxID, &shouldStop); cleanupErr != nil {
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

	command, err := buildCommand(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{}, err
	}
	commandText := commandScript(command)
	commandEnv, strippedAuthEnv := vercelSandboxCommandEnv(req.Env)
	if len(strippedAuthEnv) > 0 {
		fmt.Fprintf(b.rt.Stderr, "warning: provider=%s did not forward provider authentication variables: %s\n", providerName, strings.Join(strippedAuthEnv, ","))
	}
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, commandEnv)
	}
	commandStart := b.now()
	execRes, runErr := api.Exec(ctx, sandboxID, execRequest{
		Command:     commandText,
		WorkingDir:  workdir,
		Env:         commandEnv,
		TimeoutSecs: b.execTimeoutSecs(),
	}, b.rt.Stdout, b.rt.Stderr)
	commandDuration := b.now().Sub(commandStart)
	result = RunResult{
		Provider:      providerName,
		LeaseID:       leaseID,
		Slug:          slug,
		CommandText:   commandText,
		ExitCode:      execRes.ExitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
	}
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "vercel-sandbox run summary sync_skipped=true command=%s total=%s exit=%d\n", result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "vercel-sandbox run summary sync=%s command=%s total=%s exit=%d\n", syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	}
	var commandErr error
	if runErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		if result.ExitCode == 0 {
			result.ExitCode = 1
		}
		commandErr = ExitError{Code: 1, Message: fmt.Sprintf("vercel-sandbox run failed: %v", redactSecrets(runErr.Error()))}
	} else if result.ExitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		commandErr = ExitError{Code: result.ExitCode, Message: fmt.Sprintf("vercel-sandbox run exited %d", result.ExitCode)}
	}
	activityErr := b.refreshLeaseActivityIfRetained(leaseID, shouldStop)
	if activityErr != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: refresh vercel-sandbox lease activity failed lease=%s: %v\n", leaseID, activityErr)
		if commandErr == nil {
			result.ExitCode = 1
		}
	}
	if cleanupErr := b.cleanupCreatedRun(ctx, api, leaseID, sandboxID, &shouldStop); cleanupErr != nil {
		if result.ExitCode == 0 {
			result.ExitCode = 1
		}
		commandErr = errors.Join(commandErr, cleanupErr)
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
			CommandMs:     result.Command.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      result.ExitCode,
			Label:         strings.TrimSpace(req.Label),
		}); err != nil {
			return result, err
		}
	}
	if commandErr != nil {
		return result, commandErr
	}
	return result, activityErr
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	api, err := b.client()
	if err != nil {
		return nil, err
	}
	sandboxes, err := api.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	if len(sandboxes) == 0 {
		return []LeaseView{}, nil
	}
	if err := b.bindProviderScope(ctx, api, true); err != nil {
		return nil, err
	}
	views := make([]LeaseView, 0, len(sandboxes))
	for _, sb := range sandboxes {
		if len(sb.Metadata) == 0 && strings.HasPrefix(sb.ID, leasePrefix) {
			claim, err := readLeaseClaim(sb.ID)
			if err != nil {
				return nil, err
			}
			if claim.LeaseID == "" || claim.Provider != providerName || !b.claimMatchesActiveScope(claim) {
				continue
			}
			remote, err := api.GetSandbox(ctx, strings.TrimPrefix(claim.LeaseID, leasePrefix))
			if err != nil {
				if isVercelSandboxNotFound(err) {
					continue
				}
				return nil, err
			}
			sb = remote
		}
		leaseID := strings.TrimSpace(sb.Metadata[metadataClaimKey])
		if leaseID == "" {
			continue
		}
		claim, err := readLeaseClaim(leaseID)
		if err != nil {
			return nil, err
		}
		if claim.LeaseID == "" || claim.Provider != providerName {
			continue
		}
		if err := b.validateClaimScope(claim); err != nil {
			return nil, err
		}
		if err := validateSandboxOwnership(claim, sb); err != nil {
			return nil, err
		}
		views = append(views, b.serverFromSandbox(claim, sb))
	}
	return views, nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	api, err := b.client()
	if err != nil {
		return StatusView{}, err
	}
	if err := b.bindProviderScope(ctx, api, true); err != nil {
		return StatusView{}, err
	}
	leaseID, sandboxID, slug, err := b.resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return StatusView{}, err
	}
	claim, ok, err := b.resolveVercelSandboxLeaseClaim(leaseID)
	if err != nil {
		return StatusView{}, err
	}
	if !ok {
		return StatusView{}, exit(4, "vercel-sandbox sandbox %q is not claimed by Crabbox", req.ID)
	}
	waitTimeout := req.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = 5 * time.Minute
	}
	deadline := b.now().Add(waitTimeout)
	pollCtx := ctx
	cancel := func() {}
	if req.Wait {
		pollCtx, cancel = context.WithTimeout(ctx, waitTimeout)
	}
	defer cancel()
	for {
		sb, getErr := api.GetSandbox(pollCtx, sandboxID)
		if getErr != nil {
			if req.Wait && ctx.Err() == nil && pollCtx.Err() != nil {
				return StatusView{}, exit(5, "timed out waiting for vercel-sandbox sandbox %s to become ready", sandboxID)
			}
			if ctx.Err() != nil {
				return StatusView{}, ctx.Err()
			}
			return StatusView{}, getErr
		}
		if err := validateSandboxOwnership(claim, sb); err != nil {
			return StatusView{}, err
		}
		state := normalizedSandboxState(sb)
		view := StatusView{
			ID:       leaseID,
			Slug:     slug,
			Provider: providerName,
			TargetOS: targetLinux,
			State:    state,
			ServerID: sandboxID,
			Pond:     claim.Pond,
			Network:  NetworkPublic,
			Ready:    isReadyState(state),
			Labels: map[string]string{
				"provider": providerName,
				"lease":    leaseID,
				"slug":     slug,
				"pond":     claim.Pond,
				"state":    state,
			},
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if isTerminalState(state) {
			return StatusView{}, exit(5, "vercel-sandbox sandbox %s entered terminal state %q before becoming ready", sandboxID, state)
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for vercel-sandbox sandbox %s to become ready", sandboxID)
		}
		select {
		case <-pollCtx.Done():
			if ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for vercel-sandbox sandbox %s to become ready", sandboxID)
			}
			return StatusView{}, pollCtx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	api, err := b.client()
	if err != nil {
		return err
	}
	if err := b.bindProviderScope(ctx, api, true); err != nil {
		return err
	}
	leaseID, _, _, err := b.resolveLeaseID(req.ID, "", false, 0)
	if err != nil {
		return err
	}
	unlockOperation, err := lockVercelSandboxLeaseOperation(ctx, leaseID)
	if err != nil {
		return err
	}
	defer unlockOperation()
	leaseID, sandboxID, _, err := b.resolveLeaseID(leaseID, "", false, 0)
	if err != nil {
		return err
	}
	if _, err := b.verifyClaim(ctx, api, leaseID, sandboxID); err != nil {
		if !isVercelSandboxNotFound(err) || !b.cfg.VercelSandbox.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing vercel-sandbox sandbox=%s after explicit request\n", sandboxID)
		removeLeaseClaim(leaseID)
		return nil
	}
	if err := api.DeleteSandbox(ctx, sandboxID); err != nil {
		if !isVercelSandboxNotFound(err) || !b.cfg.VercelSandbox.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing vercel-sandbox sandbox=%s after explicit request\n", sandboxID)
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

func (b *backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	api, err := b.client()
	if err != nil {
		return err
	}
	claims, err := listVercelSandboxLeaseClaims()
	if err != nil {
		return err
	}
	hasProviderClaims := slices.ContainsFunc(claims, func(claim LeaseClaim) bool {
		return claim.Provider == providerName
	})
	if !hasProviderClaims {
		if !req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=0 claims_removed=0 checked=0\n", providerName)
		}
		return nil
	}
	if err := b.bindProviderScope(ctx, api, true); err != nil {
		return err
	}
	now := b.now().UTC()
	checked := 0
	removed := 0
	claimsRemoved := 0
	for _, listed := range claims {
		if listed.Provider != providerName || !b.claimMatchesActiveScope(listed) {
			continue
		}
		var removedOne, claimRemovedOne, checkedOne bool
		err := func() error {
			unlockOperation, err := lockVercelSandboxLeaseOperation(ctx, listed.LeaseID)
			if err != nil {
				return err
			}
			defer unlockOperation()
			claim, err := readLeaseClaim(listed.LeaseID)
			if err != nil {
				return err
			}
			if claim.LeaseID == "" || claim.Provider != providerName || !b.claimMatchesActiveScope(claim) {
				return nil
			}
			checkedOne = true
			sandboxID := strings.TrimPrefix(claim.LeaseID, leasePrefix)
			sb, getErr := api.GetSandbox(ctx, sandboxID)
			if getErr != nil {
				if !isVercelSandboxNotFound(getErr) {
					return getErr
				}
				if !b.cfg.VercelSandbox.ForgetMissing {
					fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=missing-or-inaccessible; set vercelSandbox.forgetMissing to remove the claim\n", sandboxID, claim.LeaseID)
					return nil
				}
				if req.DryRun {
					fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s slug=%s reason=missing sandbox\n", claim.LeaseID, blank(claim.Slug, "-"))
					return nil
				}
				if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
					return err
				}
				fmt.Fprintf(b.rt.Stdout, "remove claim lease=%s slug=%s reason=missing sandbox\n", claim.LeaseID, blank(claim.Slug, "-"))
				claimRemovedOne = true
				return nil
			}
			due, reason := claimCleanupDue(claim, now)
			if !due {
				fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
				return nil
			}
			if err := validateSandboxOwnership(claim, sb); err != nil {
				return err
			}
			if req.DryRun {
				fmt.Fprintf(b.rt.Stdout, "would delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
				return nil
			}
			if err := api.DeleteSandbox(ctx, sandboxID); err != nil && !isVercelSandboxNotFound(err) {
				return err
			}
			if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
				return err
			}
			fmt.Fprintf(b.rt.Stdout, "delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
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

func (b *backend) createSandbox(ctx context.Context, api vercelSandboxClient, repo Repo, reclaim bool, requestedSlug string, retained bool) (string, string, string, func(), error) {
	if err := validateVercelSandboxConfig(b.cfg); err != nil {
		return "", "", "", nil, err
	}
	providerScope, err := b.newClaimScope()
	if err != nil {
		return "", "", "", nil, err
	}
	if _, err := vercelSandboxWorkdir(b.cfg); err != nil {
		return "", "", "", nil, err
	}
	name := newSandboxName(repo)
	tentativeLeaseID := leasePrefix + name
	slug, err := allocateClaimLeaseSlug(tentativeLeaseID, requestedSlug)
	if err != nil {
		return "", "", "", nil, err
	}
	initialMetadata := b.ownershipMetadata(providerScope, tentativeLeaseID, slug, repo)
	sb, err := api.CreateSandbox(ctx, createSandboxRequest{
		Name:       name,
		Persistent: retained || b.cfg.VercelSandbox.Persistent,
		Metadata:   initialMetadata,
	})
	if err != nil {
		return "", "", "", nil, err
	}
	leaseID := leasePrefix + sb.ID
	unlockOperation, err := lockVercelSandboxLeaseOperation(ctx, leaseID)
	if err != nil {
		return leaseID, sb.ID, "", nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	keepLock := false
	defer func() {
		if !keepLock {
			unlockOperation()
		}
	}()
	if leaseID != tentativeLeaseID {
		slug, err = allocateClaimLeaseSlug(leaseID, requestedSlug)
		if err != nil {
			return leaseID, sb.ID, "", nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
		}
	}
	metadata := b.ownershipMetadata(providerScope, leaseID, slug, repo)
	if leaseID != tentativeLeaseID {
		sb, err = api.UpdateSandboxMetadata(ctx, sb.ID, metadata)
		if err != nil {
			return leaseID, sb.ID, slug, nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
		}
	} else if sb.Metadata == nil || sb.Metadata[metadataClaimKey] == "" {
		sb.Metadata = metadata
	}
	if err := validateSandboxOwnership(LeaseClaim{LeaseID: leaseID, Provider: providerName, ProviderScope: providerScope}, sb); err != nil {
		return leaseID, sb.ID, slug, nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, providerScope, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		return leaseID, sb.ID, slug, nil, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	keepLock = true
	return leaseID, sb.ID, slug, unlockOperation, nil
}

func (b *backend) ownershipMetadata(providerScope, leaseID, slug string, repo Repo) map[string]string {
	out := map[string]string{
		metadataProviderKey: providerName,
		metadataScopeKey:    providerScope,
		metadataRepoKey:     repoScope(repo),
	}
	if leaseID != "" {
		out[metadataClaimKey] = leaseID
	}
	if slug != "" {
		out[metadataSlugKey] = slug
	}
	return out
}

func (b *backend) serverFromSandbox(claim LeaseClaim, sb sandboxSummary) Server {
	state := normalizedSandboxState(sb)
	return Server{
		Provider: providerName,
		CloudID:  sb.ID,
		Name:     sb.ID,
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

func (b *backend) resolveLeaseID(id, repoRoot string, reclaim bool, idleTimeout time.Duration) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", exit(2, "provider=vercel-sandbox requires a Crabbox-created sandbox slug or lease id")
	}
	exactLeaseID := id
	if !strings.HasPrefix(exactLeaseID, leasePrefix) {
		exactLeaseID = leasePrefix + exactLeaseID
	}
	if claim, err := readLeaseClaim(exactLeaseID); err != nil {
		return "", "", "", err
	} else if claim.LeaseID == exactLeaseID && claim.Provider == providerName {
		return b.finishResolvedLease(claim, repoRoot, reclaim, idleTimeout)
	}
	claim, ok, err := b.resolveVercelSandboxLeaseClaim(id)
	if err != nil {
		return "", "", "", err
	}
	if ok {
		return b.finishResolvedLease(claim, repoRoot, reclaim, idleTimeout)
	}
	return "", "", "", exit(4, "vercel-sandbox sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-id>", id, leasePrefix)
}

func (b *backend) resolveVercelSandboxLeaseClaim(identifier string) (LeaseClaim, bool, error) {
	claims, err := listVercelSandboxLeaseClaims()
	if err != nil {
		return LeaseClaim{}, false, err
	}
	for _, claim := range claims {
		if claim.Provider == providerName && claim.LeaseID == identifier {
			if err := b.validateClaimScope(claim); err != nil {
				return LeaseClaim{}, false, err
			}
			return claim, true, nil
		}
	}
	slug := normalizeLeaseSlug(identifier)
	if slug != "" {
		for _, legacy := range []bool{false, true} {
			for _, claim := range claims {
				if claim.Provider != providerName || normalizeLeaseSlug(claim.Slug) != slug {
					continue
				}
				isLegacy := !claimMatchesScope(claim, b.providerScopeBase())
				if isLegacy != legacy || !b.claimMatchesActiveScope(claim) {
					continue
				}
				return claim, true, nil
			}
		}
	}
	return LeaseClaim{}, false, nil
}

func (b *backend) finishResolvedLease(claim LeaseClaim, repoRoot string, reclaim bool, idleTimeout time.Duration) (string, string, string, error) {
	if err := b.validateClaimScope(claim); err != nil {
		return "", "", "", err
	}
	if repoRoot != "" {
		if err := claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, claim.Pond, repoRoot,
			timeoutOrDefault(idleTimeout, time.Duration(claim.IdleTimeoutSeconds)*time.Second), reclaim); err != nil {
			return "", "", "", err
		}
	}
	slug := claim.Slug
	if strings.TrimSpace(slug) == "" {
		slug = newLeaseSlug(claim.LeaseID)
	}
	return claim.LeaseID, strings.TrimPrefix(claim.LeaseID, leasePrefix), slug, nil
}

func (b *backend) newClaimScope() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", exit(5, "generate vercel-sandbox ownership token: %v", err)
	}
	return b.providerScopeBase() + "/ownership:" + hex.EncodeToString(token[:]), nil
}

func (b *backend) bindProviderScope(ctx context.Context, api vercelSandboxClient, readOnly bool) error {
	previousScopeBase := b.providerScopeBase()
	scope, err := api.ResolveProjectScope(ctx, readOnly)
	if err != nil {
		return err
	}
	b.resolvedProject = strings.TrimSpace(scope.ProjectID)
	b.resolvedTeam = strings.TrimSpace(scope.TeamID)
	if resolvedScopeBase := b.providerScopeBase(); previousScopeBase != resolvedScopeBase && b.legacyScopeBase == "" {
		b.legacyScopeBase = previousScopeBase
	}
	return nil
}

func (b *backend) providerScopeBase() string {
	projectID := blank(strings.TrimSpace(b.resolvedProject), strings.TrimSpace(b.cfg.VercelSandbox.ProjectID))
	teamID := blank(strings.TrimSpace(b.resolvedTeam), strings.TrimSpace(b.cfg.VercelSandbox.TeamID))
	parts := []string{
		"scope:" + blank(strings.TrimSpace(b.cfg.VercelSandbox.Scope), "-"),
		"team:" + blank(teamID, "-"),
		"project:" + blank(projectID, "-"),
	}
	return strings.Join(parts, "/")
}

func (b *backend) validateClaimScope(claim LeaseClaim) error {
	if !b.claimMatchesActiveScope(claim) {
		return exit(4, "vercel-sandbox lease %q belongs to a different project/team/scope; restore the configuration used to create it", claim.LeaseID)
	}
	return nil
}

func (b *backend) claimMatchesActiveScope(claim LeaseClaim) bool {
	if claimMatchesScope(claim, b.providerScopeBase()) {
		return true
	}
	// Legacy claims are candidates only. Callers verify their remote ownership
	// tags through the currently authenticated project before use.
	return b.legacyScopeBase != "" && claimMatchesScope(claim, b.legacyScopeBase)
}

func claimMatchesScope(claim LeaseClaim, scopeBase string) bool {
	return strings.HasPrefix(strings.TrimSpace(claim.ProviderScope), scopeBase+"/ownership:")
}

func (b *backend) verifyClaim(ctx context.Context, api vercelSandboxClient, leaseID, sandboxID string) (sandboxSummary, error) {
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return sandboxSummary{}, err
	}
	if err := b.validateClaimScope(claim); err != nil {
		return sandboxSummary{}, err
	}
	sb, err := api.GetSandbox(ctx, sandboxID)
	if err != nil {
		return sandboxSummary{}, err
	}
	if err := validateSandboxOwnership(claim, sb); err != nil {
		return sandboxSummary{}, err
	}
	return sb, nil
}

func validateSandboxOwnership(claim LeaseClaim, sb sandboxSummary) error {
	if sb.ID == "" {
		return exit(5, "vercel-sandbox returned a sandbox without an id")
	}
	if sb.Metadata[metadataProviderKey] != providerName ||
		sb.Metadata[metadataScopeKey] != claim.ProviderScope ||
		sb.Metadata[metadataClaimKey] != claim.LeaseID {
		return exit(4, "vercel-sandbox sandbox %q ownership metadata does not match its local claim", sb.ID)
	}
	return nil
}

func claimCleanupDue(claim LeaseClaim, now time.Time) (bool, string) {
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

func (b *backend) refreshLeaseActivity(leaseID string) error {
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return err
	}
	if claim.LeaseID == "" {
		return nil
	}
	idleTimeout := timeoutOrDefault(b.cfg.IdleTimeout, time.Duration(claim.IdleTimeoutSeconds)*time.Second)
	return claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, claim.Pond, claim.RepoRoot, idleTimeout, false)
}

func (b *backend) refreshLeaseActivityIfRetained(leaseID string, shouldStop bool) error {
	if shouldStop {
		return nil
	}
	return b.refreshLeaseActivity(leaseID)
}

func (b *backend) cleanupCreateFailure(ctx context.Context, api vercelSandboxClient, sandboxID string, cause error) error {
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := api.DeleteSandbox(cleanupCtx, sandboxID); err != nil {
		if isVercelSandboxNotFound(err) {
			return cause
		}
		return errors.Join(cause, fmt.Errorf("vercel-sandbox cleanup failed for sandbox %s; delete it in the Vercel dashboard: %w", sandboxID, err))
	}
	return cause
}

func (b *backend) cleanupCreatedRun(ctx context.Context, api vercelSandboxClient, leaseID, sandboxID string, shouldStop *bool) error {
	if !*shouldStop {
		return nil
	}
	*shouldStop = false
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := api.DeleteSandbox(cleanupCtx, sandboxID); err != nil && !isVercelSandboxNotFound(err) {
		return fmt.Errorf("vercel-sandbox delete failed for %s: %w", sandboxID, err)
	}
	removeLeaseClaim(leaseID)
	return nil
}

func (b *backend) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), vercelSandboxCleanupTimeout)
}

func (b *backend) execTimeoutSecs() int {
	return b.cfg.VercelSandbox.ExecTimeoutSecs
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

func normalizedSandboxState(sb sandboxSummary) string {
	return strings.ToLower(blank(strings.TrimSpace(sb.Status), blank(strings.TrimSpace(sb.State), "unknown")))
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

func vercelSandboxRuntime(cfg Config) string {
	return blank(strings.TrimSpace(cfg.VercelSandbox.Runtime), defaultRuntime)
}

func newSandboxName(repo Repo) string {
	base := normalizeLeaseSlug(repo.Name)
	if base == "" {
		base = "crabbox"
	}
	base = strings.TrimPrefix(base, "crabbox-")
	if base == "" {
		base = "crabbox"
	}
	if len(base) > 40 {
		base = strings.Trim(base[:40], "-")
	}
	return "crabbox-" + base + "-" + randomSuffix()
}

func repoScope(repo Repo) string {
	value := strings.TrimSpace(repo.Root)
	if value == "" {
		value = strings.TrimSpace(repo.Name)
	}
	sum := sha256.Sum256([]byte(value))
	return "repo-sha256:" + hex.EncodeToString(sum[:8])
}

func randomSuffix() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:6]
	}
	return hex.EncodeToString(b[:])
}

func timeoutOrDefault(primary, fallback time.Duration) time.Duration {
	if primary > 0 {
		return primary
	}
	return fallback
}

func vercelSandboxCommandEnv(env map[string]string) (map[string]string, []string) {
	if len(env) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(env))
	var stripped []string
	for name, value := range env {
		switch name {
		case "CRABBOX_VERCEL_SANDBOX_AUTH_TOKEN", "CRABBOX_VERCEL_AUTH_TOKEN", "VERCEL_AUTH_TOKEN",
			"CRABBOX_VERCEL_SANDBOX_TOKEN", "CRABBOX_VERCEL_TOKEN", "VERCEL_TOKEN",
			"CRABBOX_VERCEL_SANDBOX_OIDC_TOKEN", "VERCEL_OIDC_TOKEN":
			stripped = append(stripped, name)
		default:
			out[name] = value
		}
	}
	slices.Sort(stripped)
	return out, stripped
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

func commandScript(command []string) string {
	return shellScriptFromArgv(command)
}

type vercelSandboxNotFoundError struct {
	err error
}

func (e *vercelSandboxNotFoundError) Error() string { return e.err.Error() }
func (e *vercelSandboxNotFoundError) Unwrap() error { return e.err }

func isVercelSandboxNotFound(err error) bool {
	var notFound *vercelSandboxNotFoundError
	if errors.As(err, &notFound) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "not found") || strings.Contains(text, "404")
}
