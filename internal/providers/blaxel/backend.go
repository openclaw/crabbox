package blaxel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
)

type processPoller interface {
	After(time.Duration) <-chan time.Time
}

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := now(b.rt)
	client, err := b.client()
	if err != nil {
		return err
	}
	leaseID, sb, slug, err := b.createSandbox(ctx, client, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s name=%s\n", leaseID, slug, providerName, sb.ID, sb.Name)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: blaxel warmup keeps the sandbox until explicit stop\n")
	}
	total := now(b.rt).Sub(started)
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

func (b *backend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	workdir, err := blaxelWorkdir(b.cfg)
	if err != nil {
		return RunResult{}, err
	}
	started := now(b.rt)
	client, err := b.client()
	if err != nil {
		return RunResult{}, err
	}
	leaseID, sandboxID, slug := "", "", ""
	acquired := false
	if req.ID == "" {
		var sb Sandbox
		leaseID, sb, slug, err = b.createSandbox(ctx, client, req.Repo, req.Reclaim, req.RequestedSlug)
		if err != nil {
			return RunResult{}, err
		}
		sandboxID = sb.ID
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s name=%s\n", leaseID, slug, providerName, sb.ID, sb.Name)
		acquired = true
	} else {
		leaseID, sandboxID, slug, err = resolveLeaseID(req.ID, req.Repo.Root, req.Reclaim, b.cfg.IdleTimeout, client.BaseURL(), b.cfg.Blaxel.Workspace)
		if err != nil {
			return RunResult{}, err
		}
		if _, err := verifyBlaxelClaim(ctx, client, leaseID, sandboxID, b.cfg.Blaxel.Workspace); err != nil {
			return RunResult{}, err
		}
	}
	shouldStop := acquired && !req.Keep
	if shouldStop {
		defer func() {
			if !shouldStop {
				return
			}
			cleanupCtx, cancel := b.cleanupContext(ctx)
			defer cancel()
			if err := client.DeleteSandbox(cleanupCtx, sandboxID); err != nil && !isBlaxelNotFound(err) {
				fmt.Fprintf(b.rt.Stderr, "warning: blaxel delete failed for %s: %v\n", sandboxID, err)
				return
			}
			removeLeaseClaim(leaseID)
		}()
	}
	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s sandbox=%s workdir=%s\n", providerName, leaseID, sandboxID, workdir)

	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, client, sandboxID, req, workdir)
		if err != nil {
			handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
			return RunResult{Total: now(b.rt).Sub(started), SyncDelegated: true}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.ensureWorkspace(ctx, client, sandboxID, workdir); err != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return RunResult{}, err
	}
	if req.SyncOnly {
		result := RunResult{
			Total:         now(b.rt).Sub(started),
			SyncDelegated: true,
			Provider:      providerName,
			LeaseID:       leaseID,
			Slug:          slug,
			Session:       blaxelRunSession(leaseID, slug, acquired, shouldStop),
		}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workdir)
		if req.TimingJSON {
			return result, writeTimingJSON(b.rt.Stderr, timingReport{
				Provider:      providerName,
				LeaseID:       leaseID,
				Slug:          slug,
				SyncDelegated: true,
				SyncMs:        syncDuration.Milliseconds(),
				SyncPhases:    syncPhases,
				SyncSkipped:   req.NoSync,
				TotalMs:       result.Total.Milliseconds(),
				ExitCode:      0,
				Label:         strings.TrimSpace(req.Label),
			})
		}
		return result, nil
	}

	command, err := buildCommand(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{}, err
	}
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	commandStarted := now(b.rt)
	exitCode, commandErr := b.execCommand(ctx, client, sandboxID, workdir, command, req.Env)
	commandDuration := now(b.rt).Sub(commandStarted)
	result := RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         now(b.rt).Sub(started),
		SyncDelegated: true,
		Session:       blaxelRunSession(leaseID, slug, acquired, shouldStop),
		Provider:      providerName,
		LeaseID:       leaseID,
		Slug:          slug,
		CommandText:   strings.Join(req.Command, " "),
	}
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "blaxel run summary sync_skipped=true command=%s total=%s exit=%d\n",
			result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "blaxel run summary sync=%s command=%s total=%s exit=%d\n",
			syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
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
			ExitCode:      exitCode,
			Label:         strings.TrimSpace(req.Label),
		}); err != nil {
			return result, err
		}
	}
	if commandErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		result.Session.Kept = !shouldStop
		return result, ExitError{Code: 1, Message: fmt.Sprintf("blaxel run failed: %v", commandErr)}
	}
	if exitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		result.Session.Kept = !shouldStop
		return result, ExitError{Code: exitCode, Message: fmt.Sprintf("blaxel run exited %d", exitCode)}
	}
	result.Session.Kept = !shouldStop
	return result, nil
}

func blaxelRunSession(leaseID, slug string, acquired, shouldStop bool) *RunSessionHandle {
	return &RunSessionHandle{
		Provider:       providerName,
		LeaseID:        leaseID,
		Slug:           slug,
		Reused:         !acquired,
		Kept:           !shouldStop,
		CleanupCommand: fmt.Sprintf("crabbox stop --provider %s %s", providerName, leaseID),
	}
}

func (b *backend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := b.client()
	if err != nil {
		return nil, err
	}
	claims, err := listBlaxelLeaseClaims()
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider != providerName || !blaxelClaimMatchesEndpointWorkspace(claim, client.BaseURL(), b.cfg.Blaxel.Workspace) {
			continue
		}
		sandboxID := blaxelSandboxID(claim.LeaseID)
		state := "missing-or-inaccessible"
		if sb, getErr := client.GetSandbox(ctx, sandboxID); getErr != nil {
			if !isBlaxelNotFound(getErr) {
				return nil, getErr
			}
		} else {
			if err := validateBlaxelSandboxOwnership(claim, sb); err != nil {
				return nil, err
			}
			state = blank(sb.Status, "unknown")
		}
		servers = append(servers, Server{
			Provider: providerName,
			CloudID:  sandboxID,
			Name:     sandboxID,
			Status:   state,
			Labels: map[string]string{
				"provider": providerName,
				"lease":    claim.LeaseID,
				"slug":     claim.Slug,
				"pond":     claim.Pond,
				"target":   targetLinux,
				"state":    state,
			},
		})
	}
	return servers, nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	client, err := b.client()
	if err != nil {
		return StatusView{}, err
	}
	leaseID, sandboxID, slug, err := resolveLeaseID(req.ID, "", false, 0, client.BaseURL(), b.cfg.Blaxel.Workspace)
	if err != nil {
		return StatusView{}, err
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return StatusView{}, err
	}
	waitTimeout := req.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = blaxelReadyTimeout
	}
	deadline := now(b.rt).Add(waitTimeout)
	pollCtx := ctx
	cancel := func() {}
	if req.Wait {
		pollCtx, cancel = context.WithTimeout(ctx, waitTimeout)
	}
	defer cancel()
	for {
		sb, getErr := client.GetSandbox(pollCtx, sandboxID)
		if getErr != nil {
			if req.Wait && errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for blaxel sandbox %s to become ready", sandboxID)
			}
			if ctx.Err() != nil {
				return StatusView{}, ctx.Err()
			}
			return StatusView{}, getErr
		}
		if err := validateBlaxelSandboxOwnership(claim, sb); err != nil {
			return StatusView{}, err
		}
		state := strings.ToLower(strings.TrimSpace(sb.Status))
		view := StatusView{
			ID:       leaseID,
			Slug:     slug,
			Provider: providerName,
			TargetOS: targetLinux,
			State:    state,
			ServerID: sandboxID,
			Host:     sb.Endpoint,
			Pond:     claim.Pond,
			Network:  networkPublic,
			Ready:    isReadyState(state),
			Labels: map[string]string{
				"provider": providerName,
				"lease":    leaseID,
				"pond":     claim.Pond,
				"state":    state,
			},
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if isTerminalState(state) {
			return StatusView{}, exit(5, "blaxel sandbox %s entered terminal state %q before becoming ready", sandboxID, state)
		}
		if now(b.rt).After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for blaxel sandbox %s to become ready", sandboxID)
		}
		select {
		case <-pollCtx.Done():
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for blaxel sandbox %s to become ready", sandboxID)
			}
			return StatusView{}, pollCtx.Err()
		case <-time.After(blaxelStatusPoll):
		}
	}
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	client, err := b.client()
	if err != nil {
		return err
	}
	leaseID, sandboxID, _, err := resolveLeaseID(req.ID, "", false, 0, client.BaseURL(), b.cfg.Blaxel.Workspace)
	if err != nil {
		return err
	}
	if _, err := verifyBlaxelClaim(ctx, client, leaseID, sandboxID, b.cfg.Blaxel.Workspace); err != nil {
		if !isBlaxelNotFound(err) || !b.cfg.Blaxel.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing blaxel sandbox=%s after explicit request\n", sandboxID)
		removeLeaseClaim(leaseID)
		return nil
	}
	if err := client.DeleteSandbox(ctx, sandboxID); err != nil {
		if !isBlaxelNotFound(err) || !b.cfg.Blaxel.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing blaxel sandbox=%s after explicit request\n", sandboxID)
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxID)
	return nil
}

func (b *backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	client, err := b.client()
	if err != nil {
		return err
	}
	claims, err := listBlaxelCleanupClaims()
	if err != nil {
		return err
	}
	now := now(b.rt).UTC()
	checked, removed, claimsRemoved := 0, 0, 0
	for _, claim := range claims {
		if claim.Provider != providerName || !blaxelClaimMatchesEndpointWorkspace(claim, client.BaseURL(), b.cfg.Blaxel.Workspace) {
			continue
		}
		checked++
		if strings.HasPrefix(claim.LeaseID, recoveryPrefix) {
			removedOne, claimRemovedOne, err := b.cleanupBlaxelRecovery(ctx, client, claim, now, req.DryRun)
			if err != nil {
				return err
			}
			if removedOne {
				removed++
			}
			if claimRemovedOne {
				claimsRemoved++
			}
			continue
		}
		sandboxID := blaxelSandboxID(claim.LeaseID)
		sb, getErr := client.GetSandbox(ctx, sandboxID)
		if getErr != nil {
			if !isBlaxelNotFound(getErr) {
				return getErr
			}
			if !b.cfg.Blaxel.ForgetMissing {
				fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=missing-or-inaccessible; set blaxel forget-missing to remove the claim\n", sandboxID, claim.LeaseID)
				continue
			}
			if req.DryRun {
				fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s slug=%s reason=missing sandbox\n", claim.LeaseID, blank(claim.Slug, "-"))
				continue
			}
			if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
				return err
			}
			fmt.Fprintf(b.rt.Stdout, "remove claim lease=%s slug=%s reason=missing sandbox\n", claim.LeaseID, blank(claim.Slug, "-"))
			claimsRemoved++
			continue
		}
		due, reason := blaxelClaimCleanupDue(claim, now)
		if !due {
			fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
			continue
		}
		if err := validateBlaxelSandboxOwnership(claim, sb); err != nil {
			return err
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
			continue
		}
		if err := client.DeleteSandbox(ctx, sandboxID); err != nil && !isBlaxelNotFound(err) {
			return err
		}
		if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			return err
		}
		fmt.Fprintf(b.rt.Stdout, "delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
		removed++
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d claims_removed=%d checked=%d\n", providerName, removed, claimsRemoved, checked)
	}
	return nil
}

func (b *backend) cleanupBlaxelRecovery(ctx context.Context, client Client, claim LeaseClaim, now time.Time, dryRun bool) (bool, bool, error) {
	matches := []Sandbox{}
	cursor := ""
	for {
		result, err := client.ListSandboxes(ctx, ListSandboxesRequest{
			Cursor: cursor,
			Limit:  200,
			Labels: map[string]string{blaxelClaimKey: claim.ProviderScope},
		})
		if err != nil {
			return false, false, err
		}
		for _, sb := range result.Sandboxes {
			if sb.Labels[blaxelClaimKey] != claim.ProviderScope ||
				sb.Labels["crabbox"] != "true" ||
				sb.Labels["crabbox.provider"] != providerName {
				continue
			}
			if strings.TrimSpace(sb.ID) == "" {
				return false, false, exit(5, "blaxel recovery %s matched a sandbox without an id", claim.LeaseID)
			}
			matches = append(matches, sb)
		}
		cursor = strings.TrimSpace(result.Next)
		if cursor == "" {
			break
		}
	}
	if len(matches) == 0 {
		expired, err := blaxelRecoveryExpired(claim, now)
		if err != nil {
			return false, false, err
		}
		if !expired {
			fmt.Fprintf(b.rt.Stderr, "skip recovery=%s reason=awaiting sandbox visibility or expiration\n", claim.LeaseID)
			return false, false, nil
		}
		if dryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove recovery=%s reason=sandbox lifetime elapsed\n", claim.LeaseID)
			return false, false, nil
		}
		if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			return false, false, err
		}
		fmt.Fprintf(b.rt.Stdout, "remove recovery=%s reason=sandbox lifetime elapsed\n", claim.LeaseID)
		return false, true, nil
	}
	if dryRun {
		for _, sb := range matches {
			fmt.Fprintf(b.rt.Stdout, "would delete sandbox=%s recovery=%s reason=ambiguous create\n", sb.ID, claim.LeaseID)
		}
		return false, false, nil
	}
	for _, sb := range matches {
		if err := client.DeleteSandbox(ctx, sb.ID); err != nil && !isBlaxelNotFound(err) {
			return false, false, err
		}
	}
	if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
		return false, false, err
	}
	for _, sb := range matches {
		fmt.Fprintf(b.rt.Stdout, "delete sandbox=%s recovery=%s reason=ambiguous create\n", sb.ID, claim.LeaseID)
	}
	return true, false, nil
}

func blaxelRecoveryExpired(claim LeaseClaim, now time.Time) (bool, error) {
	createdAt, err := time.Parse(time.RFC3339, strings.TrimSpace(claim.ClaimedAt))
	if err != nil {
		return false, exit(5, "blaxel recovery %s has invalid claimed time", claim.LeaseID)
	}
	if claim.IdleTimeoutSeconds <= 0 {
		return false, exit(5, "blaxel recovery %s has no sandbox lifetime", claim.LeaseID)
	}
	return !now.Before(createdAt.Add(time.Duration(claim.IdleTimeoutSeconds) * time.Second)), nil
}

func (b *backend) client() (Client, error) {
	if b.clientFactory == nil {
		return nil, exit(2, "blaxel client factory unavailable")
	}
	return b.clientFactory(b.cfg, b.rt)
}

func (b *backend) createSandbox(ctx context.Context, client Client, repo Repo, reclaim bool, requestedSlug string) (string, Sandbox, string, error) {
	claimScope, err := newBlaxelClaimScope(client.BaseURL(), b.cfg.Blaxel.Workspace)
	if err != nil {
		return "", Sandbox{}, "", err
	}
	leaseID := ""
	slug := ""
	req := CreateSandboxRequest{
		Name:       newSandboxName(repo),
		Image:      blank(b.cfg.Blaxel.Image, defaultImage),
		Region:     b.cfg.Blaxel.Region,
		MemoryMB:   b.cfg.Blaxel.MemoryMB,
		TTL:        b.cfg.Blaxel.TTL,
		IdleTTL:    b.cfg.Blaxel.IdleTTL,
		WorkingDir: blank(b.cfg.Blaxel.Workdir, defaultWorkdir),
		Labels: map[string]string{
			"crabbox":          "true",
			"crabbox.provider": providerName,
			blaxelClaimKey:     claimScope,
		},
	}
	sb, err := client.CreateSandbox(ctx, req)
	if err != nil {
		return "", Sandbox{}, "", err
	}
	if strings.TrimSpace(sb.ID) == "" {
		return "", Sandbox{}, "", b.cleanupCreateFailure(ctx, client, sb.ID, "", claimScope, repo, errors.New("blaxel create response omitted sandbox id"))
	}
	sandboxID := sb.ID
	leaseID = blaxelLeaseID(sandboxID)
	slug, err = allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return leaseID, sb, "", b.cleanupCreateFailure(ctx, client, sandboxID, "", claimScope, repo, err)
	}
	labels := blaxelLabels(leaseID, slug, claimScope, repo)
	sb, err = client.UpdateSandboxLabels(ctx, sb.ID, labels)
	if err != nil {
		return leaseID, sb, slug, b.cleanupCreateFailure(ctx, client, sandboxID, "", claimScope, repo, err)
	}
	if strings.TrimSpace(sb.ID) == "" {
		sb.ID = sandboxID
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, claimScope, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		return leaseID, sb, slug, b.cleanupCreateFailure(ctx, client, sandboxID, "", claimScope, repo, err)
	}
	ready, err := b.waitSandboxReady(ctx, client, sandboxID)
	if err != nil {
		return leaseID, sb, slug, b.cleanupCreateFailure(ctx, client, sandboxID, leaseID, claimScope, repo, err)
	}
	if len(ready.Labels) == 0 {
		ready.Labels = labels
	}
	return leaseID, ready, slug, nil
}

func (b *backend) waitSandboxReady(ctx context.Context, client Client, sandboxID string) (Sandbox, error) {
	timeout := blaxelReadyTimeout
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		sb, err := client.GetSandbox(pollCtx, sandboxID)
		if err != nil {
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return Sandbox{}, exit(5, "timed out waiting for blaxel sandbox %s to become ready", sandboxID)
			}
			return Sandbox{}, err
		}
		state := strings.ToLower(strings.TrimSpace(sb.Status))
		if isReadyState(state) || state == "" {
			return sb, nil
		}
		if isTerminalState(state) {
			return Sandbox{}, exit(5, "blaxel sandbox %s entered terminal state %q before becoming ready", sandboxID, state)
		}
		select {
		case <-pollCtx.Done():
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return Sandbox{}, exit(5, "timed out waiting for blaxel sandbox %s to become ready", sandboxID)
			}
			return Sandbox{}, pollCtx.Err()
		case <-time.After(blaxelStatusPoll):
		}
	}
}

func (b *backend) execCommand(ctx context.Context, client Client, sandboxID, workdir string, command []string, env map[string]string) (int, error) {
	if len(command) == 0 {
		return 2, errors.New("missing command")
	}
	timeout := b.execTimeoutSecs()
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second+time.Second)
	defer cancel()
	process, err := client.ExecuteProcess(waitCtx, sandboxID, ExecuteProcessRequest{
		Command:     command[0],
		Args:        command[1:],
		WorkingDir:  workdir,
		Env:         env,
		TimeoutSecs: timeout,
	})
	if err != nil {
		return 1, err
	}
	process, err = b.waitProcess(waitCtx, client, sandboxID, process)
	if err != nil {
		return 1, err
	}
	logs, err := client.GetProcessLogs(waitCtx, sandboxID, process.ID)
	if err != nil {
		return 1, err
	}
	if logs.Stdout != "" {
		_, _ = io.WriteString(b.rt.Stdout, logs.Stdout)
	}
	if logs.Stderr != "" {
		_, _ = io.WriteString(b.rt.Stderr, logs.Stderr)
	}
	if process.ExitCode == nil {
		return 1, fmt.Errorf("blaxel process %s completed without an exit code", process.ID)
	}
	return *process.ExitCode, nil
}

func (b *backend) waitProcess(ctx context.Context, client Client, sandboxID string, process Process) (Process, error) {
	if strings.TrimSpace(process.ID) == "" {
		return Process{}, errors.New("blaxel process response omitted process id")
	}
	for {
		if isProcessTerminal(process.Status) {
			return process, nil
		}
		next, err := client.GetProcess(ctx, sandboxID, process.ID)
		if err != nil {
			return Process{}, err
		}
		process = next
		if isProcessTerminal(process.Status) {
			return process, nil
		}
		select {
		case <-ctx.Done():
			cleanupCtx, cancel := b.cleanupContext(ctx)
			_ = client.StopProcess(cleanupCtx, sandboxID, process.ID)
			cancel()
			return Process{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (b *backend) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := blaxelCleanupTimeout
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

func (b *backend) cleanupCreateFailure(ctx context.Context, client Client, sandboxID, localLeaseID, claimScope string, repo Repo, cause error) error {
	if strings.TrimSpace(sandboxID) == "" {
		return cause
	}
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := client.DeleteSandbox(cleanupCtx, sandboxID); err != nil && !isBlaxelNotFound(err) {
		if strings.TrimSpace(localLeaseID) != "" {
			return errors.Join(cause, fmt.Errorf("blaxel cleanup failed for sandbox %s; local claim %s remains for cleanup: %w", sandboxID, localLeaseID, err))
		}
		recoveryID := recoveryPrefix + randomSuffix()
		if claimErr := claimLeaseForRepoProviderScopePond(recoveryID, "", providerName, claimScope, "", repo.Root, blaxelRecoveryLifetime(b.cfg), true); claimErr != nil {
			return errors.Join(cause, fmt.Errorf("blaxel cleanup failed for sandbox %s and recovery claim failed: %v; delete it in the Blaxel console: %w", sandboxID, claimErr, err))
		}
		return errors.Join(cause, fmt.Errorf("blaxel cleanup failed for sandbox %s; recovery claim %s recorded for cleanup: %w", sandboxID, recoveryID, err))
	}
	if strings.TrimSpace(localLeaseID) != "" {
		removeLeaseClaim(localLeaseID)
	}
	return cause
}

func blaxelRecoveryLifetime(cfg Config) time.Duration {
	if cfg.TTL > 0 {
		return cfg.TTL
	}
	if cfg.IdleTimeout > 0 {
		return cfg.IdleTimeout
	}
	return time.Hour
}

func (b *backend) execTimeoutSecs() int {
	if b.cfg.Blaxel.ExecTimeoutSecs > 0 {
		return b.cfg.Blaxel.ExecTimeoutSecs
	}
	return blaxelExecTimeout
}

func buildCommand(command []string, shellMode bool) ([]string, error) {
	if len(command) == 0 {
		return nil, exit(2, "missing command")
	}
	if shellMode {
		return []string{"bash", "-lc", strings.Join(command, " ")}, nil
	}
	if shouldUseShell(command) {
		return []string{"bash", "-lc", shellScriptFromArgv(command)}, nil
	}
	return command, nil
}

func blaxelWorkdir(cfg Config) (string, error) {
	workdir := strings.TrimSpace(blank(cfg.Blaxel.Workdir, defaultWorkdir))
	clean := path.Clean(workdir)
	if workdir == "" || !strings.HasPrefix(clean, "/") || strings.Contains(workdir, "\x00") {
		return "", exit(2, "blaxel workdir %q must be an absolute path", workdir)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace":
		return "", exit(2, "blaxel workdir %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func isReadyState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "running", "ready", "started", "active", "deployed", "standby":
		return true
	default:
		return false
	}
}

func isTerminalState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "terminated", "stopped", "failed", "error", "killed", "deleted":
		return true
	default:
		return false
	}
}

func isProcessTerminal(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "completed", "complete", "succeeded", "success", "failed", "error", "terminated", "stopped", "exited", "done":
		return true
	default:
		return false
	}
}

func isBlaxelNotFound(err error) bool {
	var apiErr apiError
	return errors.As(err, &apiErr) && apiErr.StatusCode == 404
}
