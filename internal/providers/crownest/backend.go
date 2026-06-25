package crownest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

const cleanupTimeout = 15 * time.Second
const statusPollInterval = 250 * time.Millisecond

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt, newClient: newClient}
}

type backend struct {
	spec      ProviderSpec
	cfg       Config
	rt        Runtime
	newClient func(Config, Runtime) (client, error)
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) client() (client, error) {
	if b.newClient != nil {
		return b.newClient(b.cfg, b.rt)
	}
	return newClient(b.cfg, b.rt)
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	api, err := b.client()
	if err != nil {
		return DoctorResult{}, err
	}
	if err := api.Probe(ctx); err != nil {
		return DoctorResult{}, err
	}
	return DoctorResult{
		Provider: providerName,
		Message:  "auth=ready api=ready mutation=false runtime=ready",
	}, nil
}

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return exit(2, "provider=crownest is delegated-run only and does not support Tailscale options")
	}
	started := b.now()
	api, err := b.client()
	if err != nil {
		return err
	}
	leaseID, sandboxID, slug, err := b.createSandbox(ctx, api, req.Repo, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, providerName, sandboxID)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: crownest warmup keeps the sandbox until explicit stop\n")
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
	if err := rejectDelegatedSyncOptionsForSpec(b.spec, req); err != nil {
		return RunResult{}, err
	}
	if req.NoSync {
		return RunResult{}, exit(2, "provider=crownest requires archive sync; --no-sync is not supported")
	}
	if req.SyncOnly {
		return RunResult{}, exit(2, "provider=crownest uses archive sync; --sync-only is not supported")
	}
	if req.Options.Tailscale.Enabled {
		return RunResult{}, exit(2, "provider=crownest is delegated-run only and does not support Tailscale options")
	}
	started := b.now()
	api, err := b.client()
	if err != nil {
		return RunResult{}, err
	}
	leaseID, sandboxID, slug := "", "", ""
	acquired := false
	if req.ID == "" {
		leaseID = ""
		acquired = true
	} else {
		leaseID, sandboxID, slug, err = resolveLeaseID(req.ID, req.Repo.Root, req.Reclaim, b.cfg.IdleTimeout, claimScope(api.BaseURL(), b.cfg))
		if err != nil {
			return RunResult{}, err
		}
		if _, err := api.GetSandbox(ctx, sandboxID); err != nil {
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
				if result.Session != nil {
					result.Session.Kept = true
				}
				if retErr == nil {
					retErr = cleanupErr
				} else {
					retErr = errors.Join(retErr, cleanupErr)
				}
			}
		}()
	}
	command, err := buildCommand(req.Command, req.ShellMode)
	if err != nil {
		return RunResult{}, err
	}
	commandText := commandScript(command)
	commandEnv, stripped := commandEnv(req.Env)
	if len(stripped) > 0 {
		fmt.Fprintf(b.rt.Stderr, "warning: provider=crownest did not forward provider authentication variables: %s\n", strings.Join(stripped, ","))
	}
	if len(commandEnv) > 0 {
		return RunResult{}, exit(2, "provider=crownest does not support command environment forwarding yet; run without Crabbox env forwarding")
	}
	if req.EnvSummary || strings.TrimSpace(os.Getenv("CRABBOX_ENV_ALLOW")) != "" {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "not-forwarded", req.Options.EnvAllow, commandEnv)
	}
	archive, archiveSHA, archiveBytes, syncPhases, syncDuration, err := b.prepareArchive(ctx, req)
	if err != nil {
		return RunResult{Total: b.now().Sub(started), SyncDelegated: true}, err
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()
	metadata := map[string]string{
		"crabbox.provider": providerName,
		"crabbox.repo":     repoName(req.Repo),
	}
	if slug != "" {
		metadata["crabbox.slug"] = slug
	}
	if leaseID != "" {
		metadata["crabbox.lease"] = leaseID
	}
	keepSandbox := req.Keep || req.KeepOnFailure || !acquired
	workspaceRun, err := api.CreateWorkspaceRun(ctx, createWorkspaceRunRequest{
		Command:   commandText,
		Keep:      keepSandbox,
		Metadata:  metadata,
		ProjectID: strings.TrimSpace(b.cfg.Crownest.ProjectID),
		SandboxID: sandboxID,
		Template:  strings.TrimSpace(b.cfg.Crownest.Template),
		TimeoutMS: timeoutMS(b.cfg.Crownest.TimeoutSecs),
		SourceMeta: map[string]string{
			"repo": repoName(req.Repo),
		},
	}, idempotencyKey("create", randomSuffix()))
	if err != nil {
		return RunResult{}, err
	}
	if workspaceRun.SandboxID != "" {
		sandboxID = workspaceRun.SandboxID
	}
	if acquired && sandboxID != "" {
		if err := b.claimAcquiredSandbox(ctx, api, req, leaseID, sandboxID, slug, &leaseID, &slug); err != nil {
			return RunResult{}, err
		}
	}
	transfer, err := api.CreateArchiveTransfer(ctx, workspaceRun.ID, createArchiveTransferRequest{SHA256: archiveSHA, SizeBytes: archiveBytes}, idempotencyKey("transfer", workspaceRun.ID))
	if err != nil {
		return b.setupFailure(ctx, req, api, err, started, acquired, leaseID, sandboxID, slug, &shouldStop)
	}
	if transfer.MaxSizeBytes > 0 && archiveBytes > transfer.MaxSizeBytes {
		return b.setupFailure(ctx, req, api, exit(6, "crownest archive too large: %d > %d bytes", archiveBytes, transfer.MaxSizeBytes), started, acquired, leaseID, sandboxID, slug, &shouldStop)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return b.setupFailure(ctx, req, api, exit(6, "rewind sync archive: %v", err), started, acquired, leaseID, sandboxID, slug, &shouldStop)
	}
	if err := api.UploadArchive(ctx, transfer, archive); err != nil {
		return b.setupFailure(ctx, req, api, err, started, acquired, leaseID, sandboxID, slug, &shouldStop)
	}
	if _, err := api.FinalizeArchive(ctx, workspaceRun.ID, finalizeArchiveRequest{SHA256: archiveSHA, SizeBytes: archiveBytes, UploadID: transfer.ID}, idempotencyKey("finalize", workspaceRun.ID)); err != nil {
		return b.setupFailure(ctx, req, api, err, started, acquired, leaseID, sandboxID, slug, &shouldStop)
	}
	workspaceRun, err = api.StartWorkspaceRun(ctx, workspaceRun.ID, idempotencyKey("start", workspaceRun.ID))
	if err != nil {
		return b.setupFailure(ctx, req, api, err, started, acquired, leaseID, sandboxID, slug, &shouldStop)
	}
	if workspaceRun.SandboxID != "" {
		sandboxID = workspaceRun.SandboxID
	}
	if acquired {
		if err := b.claimAcquiredSandbox(ctx, api, req, leaseID, sandboxID, slug, &leaseID, &slug); err != nil {
			return RunResult{}, err
		}
	}
	session := crownestRunSession(leaseID, slug, !acquired, req.Keep || !acquired)
	commandStart := b.now()
	cancelActiveRun := true
	var streamErr error
	defer func(runID string) {
		if !cancelActiveRun || (ctx.Err() == nil && streamErr == nil) {
			return
		}
		cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if canceled, cancelErr := api.CancelWorkspaceRun(cancelCtx, runID, idempotencyKey("cancel", runID)); cancelErr != nil {
			if retErr == nil {
				retErr = cancelErr
			} else {
				retErr = errors.Join(retErr, cancelErr)
			}
		} else if canceled.SandboxID != "" {
			sandboxID = canceled.SandboxID
		}
	}(workspaceRun.ID)
	terminal, streamErr := b.streamRun(ctx, api, workspaceRun.ID)
	commandDuration := b.now().Sub(commandStart)
	if terminal.ID == "" && ctx.Err() == nil {
		if latest, getErr := api.GetWorkspaceRun(ctx, workspaceRun.ID); getErr == nil {
			terminal = latest
		}
	}
	if terminalWorkspaceRun(terminal) {
		cancelActiveRun = false
	}
	terminalStatus := normalizedWorkspaceRunStatus(terminal.Status)
	missingExitStatusFailure := terminal.ExitCode == nil && terminalWorkspaceRun(terminal) && terminalStatus != "succeeded"
	exitCode := 0
	if terminal.ExitCode != nil {
		exitCode = *terminal.ExitCode
	} else if missingExitStatusFailure {
		exitCode = 1
	}
	result = RunResult{
		Provider:      providerName,
		LeaseID:       leaseID,
		Slug:          slug,
		CommandText:   commandText,
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
		Session:       session,
	}
	fmt.Fprintf(b.rt.Stderr, "crownest run summary sync=%s command=%s total=%s exit=%d\n",
		syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	if result.ExitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		if result.Session != nil {
			result.Session.Kept = !shouldStop
		}
	}
	if cleanupErr := b.cleanupCreatedRun(ctx, api, leaseID, sandboxID, &shouldStop); cleanupErr != nil {
		if result.ExitCode == 0 {
			result.ExitCode = 1
		}
		if result.Session != nil {
			result.Session.Kept = true
		}
		retErr = cleanupErr
	}
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReport{
			Provider:      providerName,
			LeaseID:       leaseID,
			Slug:          slug,
			SyncDelegated: true,
			SyncMs:        syncDuration.Milliseconds(),
			SyncPhases:    syncPhases,
			CommandMs:     result.Command.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      result.ExitCode,
			Label:         strings.TrimSpace(req.Label),
		}); err != nil {
			return result, err
		}
	}
	if retErr != nil {
		return result, retErr
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if streamErr != nil {
		return result, ExitError{Code: 1, Message: fmt.Sprintf("crownest stream failed: %v", streamErr)}
	}
	if missingExitStatusFailure {
		return result, exit(5, "crownest workspace run ended status=%s reason=%s class=%s without command exit code", blank(terminalStatus, "unknown"), blank(terminal.FailureReason, "unknown"), blank(terminal.FailureClass, "unknown"))
	}
	if terminalStatus == "failed" && terminal.FailureReason != "command_exit" {
		return result, exit(5, "crownest workspace run failed reason=%s class=%s", blank(terminal.FailureReason, "unknown"), blank(terminal.FailureClass, "unknown"))
	}
	if result.ExitCode != 0 {
		return result, ExitError{Code: result.ExitCode, Message: fmt.Sprintf("crownest run exited %d", result.ExitCode)}
	}
	return result, nil
}

func (b *backend) claimAcquiredSandbox(ctx context.Context, api client, req RunRequest, currentLeaseID, sandboxID, currentSlug string, leaseID, slug *string) error {
	if sandboxID == "" {
		return exit(5, "crownest workspace run did not report a sandbox id")
	}
	nextLeaseID := leasePrefix + sandboxID
	if currentLeaseID == nextLeaseID && currentSlug != "" {
		*leaseID = currentLeaseID
		*slug = currentSlug
		return nil
	}
	if currentLeaseID != "" && currentLeaseID != nextLeaseID {
		return exit(5, "crownest workspace run changed sandbox id from %s to %s", strings.TrimPrefix(currentLeaseID, leasePrefix), sandboxID)
	}
	allocatedSlug, err := allocateClaimLeaseSlug(nextLeaseID, req.RequestedSlug)
	if err != nil {
		return b.cleanupCreateFailure(ctx, api, sandboxID, err)
	}
	if err := claimLeaseForRepoProviderScopePond(nextLeaseID, allocatedSlug, providerName, claimScope(api.BaseURL(), b.cfg), b.cfg.Pond, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
		return b.cleanupCreateFailure(ctx, api, sandboxID, err)
	}
	*leaseID = nextLeaseID
	*slug = allocatedSlug
	fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s sandbox=%s\n", nextLeaseID, allocatedSlug, providerName, sandboxID)
	return nil
}

func (b *backend) setupFailure(ctx context.Context, req RunRequest, api client, cause error, started time.Time, acquired bool, leaseID, sandboxID, slug string, shouldStop *bool) (RunResult, error) {
	if acquired && sandboxID != "" && leaseID == "" {
		if err := b.claimAcquiredSandbox(ctx, api, req, leaseID, sandboxID, slug, &leaseID, &slug); err != nil {
			return RunResult{Provider: providerName, ExitCode: 1, Total: b.now().Sub(started), SyncDelegated: true}, errors.Join(cause, err)
		}
	}
	if leaseID != "" {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, shouldStop)
	}
	return RunResult{
		Provider:      providerName,
		LeaseID:       leaseID,
		Slug:          slug,
		ExitCode:      1,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
		Session:       crownestRunSession(leaseID, slug, !acquired, leaseID != "" && !*shouldStop),
	}, cause
}

func crownestRunSession(leaseID, slug string, reused, kept bool) *RunSessionHandle {
	if leaseID == "" {
		return nil
	}
	return &RunSessionHandle{
		Provider:       providerName,
		LeaseID:        leaseID,
		Slug:           slug,
		Reused:         reused,
		Kept:           kept,
		CleanupCommand: "crabbox stop --provider " + providerName + " --id " + shellQuote(blank(slug, leaseID)),
	}
}

func (b *backend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	api, err := b.client()
	if err != nil {
		return nil, err
	}
	claims, err := listCrownestLeaseClaims()
	if err != nil {
		return nil, err
	}
	views := make([]LeaseView, 0, len(claims))
	scope := claimScope(api.BaseURL(), b.cfg)
	for _, claim := range claims {
		if claim.Provider != providerName || claim.ProviderScope != scope {
			continue
		}
		state := "unknown"
		if req.Refresh {
			sb, err := api.GetSandbox(ctx, sandboxIDFromLease(claim.LeaseID))
			if err != nil {
				if isNotFound(err) {
					state = "missing"
				} else {
					return nil, err
				}
			} else {
				state = normalizedSandboxState(sb)
			}
		}
		views = append(views, serverFromClaim(claim, state))
	}
	return views, nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	api, err := b.client()
	if err != nil {
		return StatusView{}, err
	}
	leaseID, sandboxID, slug, err := resolveLeaseID(req.ID, "", false, 0, claimScope(api.BaseURL(), b.cfg))
	if err != nil {
		return StatusView{}, err
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
				return StatusView{}, exit(5, "timed out waiting for crownest sandbox %s to become ready", sandboxID)
			}
			if ctx.Err() != nil {
				return StatusView{}, ctx.Err()
			}
			return StatusView{}, getErr
		}
		state := normalizedSandboxState(sb)
		view := StatusView{
			ID:       leaseID,
			Slug:     slug,
			Provider: providerName,
			TargetOS: targetLinux,
			State:    state,
			ServerID: sandboxID,
			Ready:    isReadyState(state),
			Labels: map[string]string{
				"provider": providerName,
				"lease":    leaseID,
				"slug":     slug,
				"state":    state,
			},
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if isTerminalState(state) {
			return StatusView{}, exit(5, "crownest sandbox %s entered terminal state %q before becoming ready", sandboxID, state)
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for crownest sandbox %s to become ready", sandboxID)
		}
		select {
		case <-pollCtx.Done():
			if ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for crownest sandbox %s to become ready", sandboxID)
			}
			return StatusView{}, pollCtx.Err()
		case <-time.After(statusPollInterval):
		}
	}
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	api, err := b.client()
	if err != nil {
		return err
	}
	leaseID, sandboxID, _, err := resolveLeaseID(req.ID, "", false, 0, claimScope(api.BaseURL(), b.cfg))
	if err != nil {
		return err
	}
	if err := api.DeleteSandbox(ctx, sandboxID); err != nil {
		if !isNotFound(err) || !b.cfg.Crownest.ForgetMissing {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "warning: forgetting missing crownest sandbox=%s after explicit request\n", sandboxID)
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
	claims, err := listCrownestLeaseClaims()
	if err != nil {
		return err
	}
	scope := claimScope(api.BaseURL(), b.cfg)
	now := b.now().UTC()
	checked := 0
	removed := 0
	claimsRemoved := 0
	for _, claim := range claims {
		if claim.Provider != providerName || claim.ProviderScope != scope {
			continue
		}
		checked++
		sandboxID := sandboxIDFromLease(claim.LeaseID)
		_, err := api.GetSandbox(ctx, sandboxID)
		if err != nil {
			if !isNotFound(err) {
				return err
			}
			if !b.cfg.Crownest.ForgetMissing {
				fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=missing-or-inaccessible; set crownest forget-missing to remove the claim\n", sandboxID, claim.LeaseID)
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
		due, reason := crownestClaimCleanupDue(claim, now)
		if !due {
			fmt.Fprintf(b.rt.Stderr, "skip sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would delete sandbox=%s lease=%s reason=%s\n", sandboxID, claim.LeaseID, reason)
			continue
		}
		if err := api.DeleteSandbox(ctx, sandboxID); err != nil && !isNotFound(err) {
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

func (b *backend) createSandbox(ctx context.Context, api client, repo Repo, reclaim bool, requestedSlug string) (string, string, string, error) {
	sb, err := api.CreateSandbox(ctx, createSandboxRequest{
		ProjectID: strings.TrimSpace(b.cfg.Crownest.ProjectID),
		Template:  strings.TrimSpace(b.cfg.Crownest.Template),
		TTLMS:     ttlMS(b.cfg),
		Metadata: map[string]string{
			"crabbox.provider": providerName,
			"crabbox.repo":     repoName(repo),
		},
	})
	if err != nil {
		return "", "", "", err
	}
	leaseID := leasePrefix + sb.ID
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return leaseID, sb.ID, "", b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, claimScope(api.BaseURL(), b.cfg), b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		return leaseID, sb.ID, slug, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	return leaseID, sb.ID, slug, nil
}

func (b *backend) cleanupCreateFailure(ctx context.Context, api client, sandboxID string, cause error) error {
	if strings.TrimSpace(sandboxID) == "" {
		return cause
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := api.DeleteSandbox(cleanupCtx, sandboxID); err != nil && !isNotFound(err) {
		return errors.Join(cause, fmt.Errorf("crownest cleanup failed for sandbox %s; delete it in Crownest: %w", sandboxID, err))
	}
	return cause
}

func (b *backend) prepareArchive(ctx context.Context, req RunRequest) (*os.File, string, int64, []timingPhase, time.Duration, error) {
	start := b.now()
	syncCtx := ctx
	cancel := func() {}
	if b.cfg.Sync.Timeout > 0 {
		syncCtx, cancel = context.WithTimeout(ctx, b.cfg.Sync.Timeout)
	}
	defer cancel()
	excludes, err := syncExcludes(req.Repo.Root, b.cfg)
	if err != nil {
		return nil, "", 0, nil, 0, err
	}
	manifestStart := b.now()
	manifest, err := syncManifest(req.Repo.Root, excludes, b.cfg.Sync.Includes)
	if err != nil {
		return nil, "", 0, nil, 0, exit(6, "build sync file list: %v", err)
	}
	manifestDuration := b.now().Sub(manifestStart)
	preflightStart := b.now()
	if err := checkSyncPreflight(manifest, b.cfg, req.ForceSyncLarge, b.rt.Stderr); err != nil {
		return nil, "", 0, nil, 0, err
	}
	preflightDuration := b.now().Sub(preflightStart)
	archiveStart := b.now()
	archive, err := createPortableSyncArchive(syncCtx, req.Repo, manifest, "crabbox-crownest-sync-*.tgz")
	if err != nil {
		return nil, "", 0, nil, 0, err
	}
	archiveDuration := b.now().Sub(archiveStart)
	sum, size, err := hashArchive(archive, sha256.New())
	if err != nil {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
		return nil, "", 0, nil, 0, err
	}
	total := b.now().Sub(start)
	return archive, sum, size, []timingPhase{
		{Name: "manifest", Ms: manifestDuration.Milliseconds()},
		{Name: "preflight", Ms: preflightDuration.Milliseconds()},
		{Name: "archive", Ms: archiveDuration.Milliseconds()},
		{Name: "crownest_sync", Ms: total.Milliseconds()},
	}, total, nil
}

func hashArchive(file *os.File, h hash.Hash) (string, int64, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", 0, exit(6, "rewind sync archive: %v", err)
	}
	size, err := io.Copy(h, file)
	if err != nil {
		return "", 0, exit(6, "hash sync archive: %v", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", 0, exit(6, "rewind sync archive: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

func (b *backend) streamRun(ctx context.Context, api client, workspaceRunID string) (workspaceRun, error) {
	var afterSeq int64
	for attempts := 0; attempts < 3; attempts++ {
		body, err := api.StreamWorkspaceRunEvents(ctx, workspaceRunID, afterSeq)
		if err != nil {
			return workspaceRun{}, err
		}
		var terminal workspaceRun
		err = readSSE(body, func(event streamEvent) error {
			if event.Seq > afterSeq {
				afterSeq = event.Seq
			}
			switch event.Type {
			case "stdout":
				_, _ = io.WriteString(b.rt.Stdout, event.Data)
			case "stderr":
				_, _ = io.WriteString(b.rt.Stderr, event.Data)
			case "terminal":
				terminal = event.WorkspaceRun
			case "error":
				return exit(5, "crownest event error %s: %s", blank(event.Code, "error"), event.Message)
			}
			return nil
		})
		_ = body.Close()
		if terminal.ID != "" {
			return terminal, nil
		}
		if err != nil {
			return workspaceRun{}, err
		}
	}
	return workspaceRun{}, exit(5, "crownest event stream ended before terminal event")
}

func (b *backend) cleanupCreatedRun(ctx context.Context, api client, leaseID, sandboxID string, shouldStop *bool) error {
	if !*shouldStop || sandboxID == "" {
		return nil
	}
	*shouldStop = false
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := api.DeleteSandbox(cleanupCtx, sandboxID); err != nil && !isNotFound(err) {
		return fmt.Errorf("crownest delete failed for %s: %w", sandboxID, err)
	}
	if leaseID != "" {
		removeLeaseClaim(leaseID)
	}
	return nil
}

func resolveLeaseID(id, repoRoot string, reclaim bool, idleTimeout time.Duration, scope string) (string, string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", exit(2, "provider=crownest requires a Crabbox-created sandbox slug or lease id")
	}
	exactLeaseID := id
	if !strings.HasPrefix(exactLeaseID, leasePrefix) {
		exactLeaseID = leasePrefix + exactLeaseID
	}
	if claim, err := readLeaseClaim(exactLeaseID); err == nil && claim.LeaseID == exactLeaseID && claim.Provider == providerName {
		return finishResolvedLease(claim, repoRoot, reclaim, idleTimeout, scope)
	}
	claims, err := listCrownestLeaseClaims()
	if err != nil {
		return "", "", "", err
	}
	slug := normalizeLeaseSlug(id)
	for _, claim := range claims {
		if claim.Provider != providerName {
			continue
		}
		if claim.LeaseID == id || normalizeLeaseSlug(claim.Slug) == slug {
			return finishResolvedLease(claim, repoRoot, reclaim, idleTimeout, scope)
		}
	}
	return "", "", "", exit(4, "crownest sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-id>", id, leasePrefix)
}

func finishResolvedLease(claim LeaseClaim, repoRoot string, reclaim bool, idleTimeout time.Duration, scope string) (string, string, string, error) {
	if claim.ProviderScope != scope {
		return "", "", "", exit(4, "crownest lease %q belongs to a different API endpoint, project, or template", claim.LeaseID)
	}
	if repoRoot != "" {
		if err := claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, claim.Pond, repoRoot, timeoutOrDefault(idleTimeout, time.Duration(claim.IdleTimeoutSeconds)*time.Second), reclaim); err != nil {
			return "", "", "", err
		}
	}
	slug := claim.Slug
	if strings.TrimSpace(slug) == "" {
		slug = newLeaseSlug(claim.LeaseID)
	}
	return claim.LeaseID, sandboxIDFromLease(claim.LeaseID), slug, nil
}

func serverFromClaim(claim LeaseClaim, state string) Server {
	return Server{
		Provider: providerName,
		CloudID:  sandboxIDFromLease(claim.LeaseID),
		Name:     sandboxIDFromLease(claim.LeaseID),
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

func sandboxIDFromLease(leaseID string) string {
	return strings.TrimPrefix(leaseID, leasePrefix)
}

func claimScope(baseURL string, cfg Config) string {
	return strings.Join([]string{
		"endpoint:" + strings.TrimSpace(baseURL),
		"project:" + strings.TrimSpace(cfg.Crownest.ProjectID),
		"template:" + strings.TrimSpace(cfg.Crownest.Template),
	}, "|")
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

func commandEnv(env map[string]string) (map[string]string, []string) {
	if len(env) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(env))
	var stripped []string
	for name, value := range env {
		if isProviderAuthEnv(name) {
			stripped = append(stripped, name)
			continue
		}
		out[name] = value
	}
	sort.Strings(stripped)
	if len(out) == 0 {
		out = nil
	}
	return out, stripped
}

func isProviderAuthEnv(name string) bool {
	return name == "CRABBOX_CROWNEST_API_KEY" ||
		name == "CROWNEST_API_KEY" ||
		name == "CROWNEST" ||
		strings.HasPrefix(name, "CROWNEST_")
}

func crownestClaimCleanupDue(claim LeaseClaim, now time.Time) (bool, string) {
	if claim.IdleTimeoutSeconds <= 0 {
		return false, "idle-timeout-disabled"
	}
	lastUsedAt := strings.TrimSpace(claim.LastUsedAt)
	if lastUsedAt == "" {
		lastUsedAt = strings.TrimSpace(claim.ClaimedAt)
	}
	lastUsed, err := time.Parse(time.RFC3339, lastUsedAt)
	if err != nil {
		return false, "invalid-last-used"
	}
	deadline := lastUsed.Add(time.Duration(claim.IdleTimeoutSeconds) * time.Second)
	if now.Before(deadline) {
		return false, "idle-not-expired"
	}
	return true, "idle-expired"
}

func timeoutMS(timeoutSecs int) int64 {
	if timeoutSecs <= 0 {
		return 0
	}
	return int64(timeoutSecs) * int64(time.Second/time.Millisecond)
}

func ttlMS(cfg Config) int64 {
	if cfg.TTL <= 0 {
		return 0
	}
	return int64(cfg.TTL / time.Millisecond)
}

func timeoutOrDefault(primary, fallback time.Duration) time.Duration {
	if primary > 0 {
		return primary
	}
	return fallback
}

func normalizedSandboxState(sb sandbox) string {
	return strings.ToLower(blank(strings.TrimSpace(sb.Status), "unknown"))
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

func terminalWorkspaceRun(run workspaceRun) bool {
	switch normalizedWorkspaceRunStatus(run.Status) {
	case "succeeded", "failed", "canceled":
		return true
	default:
		return false
	}
}

func normalizedWorkspaceRunStatus(status string) string {
	return strings.TrimSpace(strings.ToLower(status))
}

func repoName(repo Repo) string {
	if strings.TrimSpace(repo.Name) != "" {
		return repo.Name
	}
	return repo.Root
}

func randomSuffix() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:6]
	}
	return hex.EncodeToString(b[:])
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}
