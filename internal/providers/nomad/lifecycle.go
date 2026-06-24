package nomad

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
)

const statusPollInterval = 2 * time.Second

type allocationReadiness struct {
	JobID         string
	AllocationID  string
	Task          string
	NodeID        string
	NodeName      string
	ClientStatus  string
	DesiredStatus string
	TaskState     string
}

func (r allocationReadiness) State() string {
	if r.AllocationID == "" {
		return "not-ready"
	}
	if r.ClientStatus == nomadapi.AllocClientStatusRunning && r.DesiredStatus == nomadapi.AllocDesiredStatusRun {
		return "running"
	}
	if isTerminalAllocationStatus(r.ClientStatus) || r.DesiredStatus != "" && r.DesiredStatus != nomadapi.AllocDesiredStatusRun {
		return "terminal"
	}
	return "not-ready"
}

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	if req.Options.Tailscale.Enabled {
		return exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	started := b.now()
	client, err := b.client()
	if err != nil {
		return err
	}
	leaseID, err := newLeaseID()
	if err != nil {
		return err
	}
	slug, err := allocateClaimLeaseSlug(leaseID, req.RequestedSlug)
	if err != nil {
		return err
	}
	expiresAt := time.Time{}
	if b.cfg.TTL > 0 {
		expiresAt = b.now().UTC().Add(b.cfg.TTL)
	}
	jobID := jobIDForLease(leaseID)
	job, err := buildJobSpec(b.cfg, jobSpecInput{LeaseID: leaseID, Slug: slug, JobID: jobID, ExpiresAt: expiresAt})
	if err != nil {
		return err
	}
	evalID, err := client.RegisterJob(ctx, job)
	if err != nil {
		return err
	}
	if evalID != "" {
		if err := b.waitForEvaluation(ctx, client, evalID); err != nil {
			return err
		}
	}
	ready, err := b.waitForAllocation(ctx, client, jobID, b.allocReadyTimeout())
	if err != nil {
		return err
	}
	if _, err := writeNomadClaim(b.cfg, leaseID, slug, req.Repo, req.Reclaim, ready, expiresAt); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s job=%s allocation=%s task=%s workdir=%s\n", leaseID, slug, providerName, jobID, ready.AllocationID, b.cfg.Nomad.Task, b.cfg.Nomad.Workdir)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: nomad warmup keeps the job until explicit stop\n")
	}
	total := b.now().Sub(started)
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", total.Round(time.Millisecond))
	return nil
}

func (b *backend) Run(ctx context.Context, req RunRequest) (result RunResult, retErr error) {
	if req.Options.Tailscale.Enabled {
		return RunResult{}, exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
	}
	if err := delegatedSyncOptionsError(b.spec, req); err != nil {
		return RunResult{}, err
	}
	if !req.SyncOnly && (len(req.Command) == 0 || len(req.Command) == 1 && strings.TrimSpace(req.Command[0]) == "") {
		return RunResult{}, exit(2, "missing command")
	}
	workdir := strings.TrimSpace(b.cfg.Nomad.Workdir)
	started := b.now()
	client, err := b.client()
	if err != nil {
		return RunResult{}, err
	}

	leaseID, slug := "", ""
	claim := LeaseClaim{}
	ready := allocationReadiness{}
	acquired := false
	if strings.TrimSpace(req.ID) == "" {
		leaseID, slug, ready, claim, err = b.createRunJob(ctx, client, req)
		if err != nil {
			return RunResult{}, err
		}
		fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s job=%s allocation=%s task=%s\n", leaseID, slug, providerName, ready.JobID, ready.AllocationID, ready.Task)
		acquired = true
	} else {
		claim, err = resolveNomadClaim(b.cfg, req.ID)
		if err != nil {
			return RunResult{}, err
		}
		leaseID, slug = claim.LeaseID, claim.Slug
		jobID := claim.Labels[claimLabelJobID]
		job, err := client.JobInfo(ctx, jobID)
		if err != nil {
			return RunResult{}, err
		}
		if err := validateRemoteOwnership(b.cfg, claim, job); err != nil {
			return RunResult{}, err
		}
		ready, err = b.waitForAllocation(ctx, client, jobID, b.allocReadyTimeout())
		if err != nil {
			return RunResult{}, err
		}
		if req.Repo.Root != "" {
			if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, claim.ProviderScope, b.cfg.Pond, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
				return RunResult{}, err
			}
			updated, err := readLeaseClaim(leaseID)
			if err != nil {
				return RunResult{}, err
			}
			claim, err = updateLeaseClaimLabelsIfUnchanged(leaseID, updated, claimLabels(b.cfg, leaseID, slug, ready, claimExpiresAt(claim)))
			if err != nil {
				return RunResult{}, err
			}
		}
	}

	shouldStop := acquired && !req.Keep
	cleanedUp := false
	session := &RunSessionHandle{
		Provider:       providerName,
		LeaseID:        leaseID,
		Slug:           slug,
		Reused:         !acquired,
		Kept:           !shouldStop,
		CleanupCommand: nomadCleanupCommand(leaseID),
	}
	finishResult := func(result RunResult) RunResult {
		if result.Provider == "" {
			result.Provider = providerName
			result.LeaseID = leaseID
			result.Slug = slug
		}
		if result.LeaseID != "" {
			result.Session = session
			result.Session.Kept = !cleanedUp && !shouldStop
		}
		return result
	}
	defer func() {
		if shouldStop {
			cleanupCtx, cancel := b.cleanupContext(ctx)
			defer cancel()
			if err := b.deleteOwnedRunJob(cleanupCtx, client, claim); err != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: nomad stop failed for lease=%s job=%s: %v\n", leaseID, claim.Labels[claimLabelJobID], err)
			} else {
				cleanedUp = true
			}
		}
		result = finishResult(result)
	}()

	fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s job=%s allocation=%s task=%s workdir=%s\n", providerName, leaseID, ready.JobID, ready.AllocationID, ready.Task, workdir)

	syncDuration := time.Duration(0)
	syncPhases := []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	if !req.NoSync {
		syncPhases, syncDuration, err = b.syncWorkspace(ctx, client, ready, req, workdir)
		if err != nil {
			handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
			return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true}, err
		}
		fmt.Fprintf(b.rt.Stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	} else if err := b.execShell(ctx, client, ready, "mkdir -p "+shellQuote(workdir)); err != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true}, err
	}

	if req.SyncOnly {
		result = RunResult{Provider: providerName, LeaseID: leaseID, Slug: slug, Total: b.now().Sub(started), SyncDelegated: true}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", workdir)
		if !shouldStop {
			if err := refreshNomadLeaseActivity(b.cfg, claim); err != nil {
				result.ExitCode = 1
				if req.TimingJSON {
					_ = writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
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
						Workdir:       workdir,
					}, result, err))
				}
				return result, err
			}
		}
		if req.TimingJSON {
			return result, writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
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
				Workdir:       workdir,
			}, result, nil))
		}
		return result, nil
	}

	commandStart := b.now()
	exitCode, runErr := b.runCommand(ctx, client, ready, req, workdir)
	commandDuration := b.now().Sub(commandStart)
	result = RunResult{
		Provider:      providerName,
		LeaseID:       leaseID,
		Slug:          slug,
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
		CommandText:   strings.Join(req.Command, " "),
	}
	fmt.Fprintf(b.rt.Stderr, "nomad run summary sync=%s command=%s total=%s exit=%d\n", syncDuration.Round(time.Millisecond), commandDuration.Round(time.Millisecond), result.Total.Round(time.Millisecond), exitCode)
	if req.TimingJSON {
		if err := writeTimingJSON(b.rt.Stderr, timingReportWithRunResult(timingReport{
			Provider:      providerName,
			LeaseID:       leaseID,
			Slug:          slug,
			SyncDelegated: true,
			SyncMs:        syncDuration.Milliseconds(),
			SyncPhases:    syncPhases,
			SyncSkipped:   req.NoSync,
			CommandMs:     commandDuration.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      exitCode,
			Label:         strings.TrimSpace(req.Label),
			Workdir:       workdir,
		}, result, runErr)); err != nil {
			return result, err
		}
	}
	if runErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			return result, runErr
		}
		return result, ExitError{Code: 1, Message: runErr.Error()}
	}
	if exitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: exitCode, Message: fmt.Sprintf("nomad run exited %d", exitCode)}
	}
	if !shouldStop {
		if err := refreshNomadLeaseActivity(b.cfg, claim); err != nil {
			result.ExitCode = 1
			return result, err
		}
	}
	return result, nil
}

func (b *backend) createRunJob(ctx context.Context, client Client, req RunRequest) (string, string, allocationReadiness, LeaseClaim, error) {
	leaseID, err := newLeaseID()
	if err != nil {
		return "", "", allocationReadiness{}, LeaseClaim{}, err
	}
	slug, err := allocateClaimLeaseSlug(leaseID, req.RequestedSlug)
	if err != nil {
		return "", "", allocationReadiness{}, LeaseClaim{}, err
	}
	expiresAt := time.Time{}
	if b.cfg.TTL > 0 {
		expiresAt = b.now().UTC().Add(b.cfg.TTL)
	}
	jobID := jobIDForLease(leaseID)
	job, err := buildJobSpec(b.cfg, jobSpecInput{LeaseID: leaseID, Slug: slug, JobID: jobID, ExpiresAt: expiresAt})
	if err != nil {
		return "", "", allocationReadiness{}, LeaseClaim{}, err
	}
	evalID, err := client.RegisterJob(ctx, job)
	if err != nil {
		return "", "", allocationReadiness{}, LeaseClaim{}, err
	}
	if evalID != "" {
		if err := b.waitForEvaluation(ctx, client, evalID); err != nil {
			return "", "", allocationReadiness{}, LeaseClaim{}, err
		}
	}
	ready, err := b.waitForAllocation(ctx, client, jobID, b.allocReadyTimeout())
	if err != nil {
		return "", "", allocationReadiness{}, LeaseClaim{}, err
	}
	claim, err := writeNomadClaim(b.cfg, leaseID, slug, req.Repo, req.Reclaim, ready, expiresAt)
	if err != nil {
		cleanupCtx, cancel := b.cleanupContext(ctx)
		defer cancel()
		if _, cleanupErr := client.DeregisterJob(cleanupCtx, jobID, true); cleanupErr != nil {
			return "", "", allocationReadiness{}, LeaseClaim{}, errors.Join(err, fmt.Errorf("cleanup nomad job %s after claim failure: %w", jobID, cleanupErr))
		}
		return "", "", allocationReadiness{}, LeaseClaim{}, err
	}
	return leaseID, slug, ready, claim, nil
}

func (b *backend) deleteOwnedRunJob(ctx context.Context, client Client, expected LeaseClaim) error {
	if expected.LeaseID == "" {
		return nil
	}
	claim, err := readLeaseClaim(expected.LeaseID)
	if err != nil {
		return err
	}
	if claim.LeaseID == "" {
		return exit(4, "nomad lease %s disappeared before release", expected.LeaseID)
	}
	if err := authorizeClaimScope(b.cfg, claim); err != nil {
		return err
	}
	jobID := claim.Labels[claimLabelJobID]
	job, err := client.JobInfo(ctx, jobID)
	if err != nil {
		if isNotFoundError(err) {
			return removeLeaseClaimIfUnchanged(claim.LeaseID, claim)
		}
		return err
	}
	if err := validateRemoteOwnership(b.cfg, claim, job); err != nil {
		return err
	}
	if _, err := client.DeregisterJob(ctx, jobID, true); err != nil {
		return err
	}
	return removeLeaseClaimIfUnchanged(claim.LeaseID, claim)
}

func refreshNomadLeaseActivity(cfg Config, claim LeaseClaim) error {
	if claim.LeaseID == "" {
		return nil
	}
	idleTimeout := cfg.IdleTimeout
	if idleTimeout <= 0 && claim.IdleTimeoutSeconds > 0 {
		idleTimeout = time.Duration(claim.IdleTimeoutSeconds) * time.Second
	}
	if err := claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, claim.Pond, claim.RepoRoot, idleTimeout, false); err != nil {
		return err
	}
	updated, err := readLeaseClaim(claim.LeaseID)
	if err != nil {
		return err
	}
	_, err = updateLeaseClaimLabelsIfUnchanged(claim.LeaseID, updated, claim.Labels)
	return err
}

func claimExpiresAt(claim LeaseClaim) time.Time {
	value := strings.TrimSpace(claim.Labels[claimLabelExpiresAt])
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func (b *backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	client, err := b.client()
	if err != nil {
		return nil, err
	}
	claims, err := listNomadLeaseClaims()
	if err != nil {
		return nil, err
	}
	views := make([]LeaseView, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider != providerName || claim.ProviderScope != claimScope(b.cfg) {
			continue
		}
		view, err := b.statusFromClaim(ctx, client, claim, false)
		if err != nil {
			return nil, err
		}
		views = append(views, Server{
			CloudID:  view.ServerID,
			Provider: providerName,
			Name:     view.ServerID,
			Status:   view.State,
			Labels:   view.Labels,
		})
	}
	return views, nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	client, err := b.client()
	if err != nil {
		return StatusView{}, err
	}
	claim, err := resolveNomadClaim(b.cfg, req.ID)
	if err != nil {
		return StatusView{}, err
	}
	waitTimeout := req.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = b.allocReadyTimeout()
	}
	pollCtx := ctx
	cancel := func() {}
	if req.Wait {
		pollCtx, cancel = context.WithTimeout(ctx, waitTimeout)
	}
	defer cancel()
	for {
		view, err := b.statusFromClaim(pollCtx, client, claim, req.Wait)
		if err != nil {
			return StatusView{}, err
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if view.State == "terminal" || view.State == "missing-or-inaccessible" {
			return StatusView{}, exit(5, "nomad job %s reached state %s before becoming ready", claim.Labels[claimLabelJobID], view.State)
		}
		select {
		case <-pollCtx.Done():
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for nomad job %s allocation readiness", claim.Labels[claimLabelJobID])
			}
			return StatusView{}, pollCtx.Err()
		case <-time.After(statusPollInterval):
		}
	}
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	client, err := b.client()
	if err != nil {
		return err
	}
	claim, err := resolveNomadClaim(b.cfg, req.ID)
	if err != nil {
		return err
	}
	jobID := claim.Labels[claimLabelJobID]
	job, err := client.JobInfo(ctx, jobID)
	if err != nil {
		if isNotFoundError(err) {
			removeLeaseClaim(claim.LeaseID)
			fmt.Fprintf(b.rt.Stderr, "removed stale nomad claim lease=%s job=%s reason=missing-or-inaccessible\n", claim.LeaseID, jobID)
			return nil
		}
		return err
	}
	if err := validateRemoteOwnership(b.cfg, claim, job); err != nil {
		return err
	}
	if _, err := client.DeregisterJob(ctx, jobID, true); err != nil {
		return err
	}
	removeLeaseClaim(claim.LeaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s job=%s\n", claim.LeaseID, jobID)
	return nil
}

func (b *backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	client, err := b.client()
	if err != nil {
		return err
	}
	claims, err := listNomadLeaseClaims()
	if err != nil {
		return err
	}
	now := b.now().UTC()
	checked, removed := 0, 0
	for _, listed := range claims {
		if listed.Provider != providerName || listed.ProviderScope != claimScope(b.cfg) {
			continue
		}
		claim, err := readLeaseClaim(listed.LeaseID)
		if err != nil {
			return err
		}
		if claim.LeaseID == "" || claim.Provider != providerName || claim.ProviderScope != claimScope(b.cfg) {
			continue
		}
		if err := authorizeClaimScope(b.cfg, claim); err != nil {
			return err
		}
		checked++
		due, reason := claimCleanupDue(claim, now)
		jobID := claim.Labels[claimLabelJobID]
		if !due {
			fmt.Fprintf(b.rt.Stderr, "skip nomad job=%s lease=%s reason=%s\n", jobID, claim.LeaseID, reason)
			continue
		}
		job, err := client.JobInfo(ctx, jobID)
		if err != nil {
			if !isNotFoundError(err) {
				return err
			}
			if req.DryRun {
				fmt.Fprintf(b.rt.Stdout, "would remove nomad claim lease=%s job=%s reason=missing-or-inaccessible\n", claim.LeaseID, jobID)
				continue
			}
			if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
				return err
			}
			fmt.Fprintf(b.rt.Stdout, "remove nomad claim lease=%s job=%s reason=missing-or-inaccessible\n", claim.LeaseID, jobID)
			removed++
			continue
		}
		if err := validateRemoteOwnership(b.cfg, claim, job); err != nil {
			return err
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would deregister nomad job=%s lease=%s reason=%s\n", jobID, claim.LeaseID, reason)
			continue
		}
		if _, err := client.DeregisterJob(ctx, jobID, true); err != nil {
			return err
		}
		if err := removeLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			return err
		}
		fmt.Fprintf(b.rt.Stdout, "deregister nomad job=%s lease=%s reason=%s\n", jobID, claim.LeaseID, reason)
		removed++
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d checked=%d\n", providerName, removed, checked)
	}
	return nil
}

func (b *backend) statusFromClaim(ctx context.Context, client Client, claim LeaseClaim, wait bool) (StatusView, error) {
	if err := authorizeClaimScope(b.cfg, claim); err != nil {
		return StatusView{}, err
	}
	jobID := claim.Labels[claimLabelJobID]
	base := StatusView{
		ID:       claim.LeaseID,
		Slug:     claim.Slug,
		Provider: providerName,
		TargetOS: targetLinux,
		ServerID: jobID,
		Pond:     claim.Pond,
		Network:  networkPublic,
		Labels:   baseStatusLabels(b.cfg, claim, "not-ready"),
	}
	job, err := client.JobInfo(ctx, jobID)
	if err != nil {
		if isNotFoundError(err) {
			base.State = "missing-or-inaccessible"
			base.Labels[claimLabelState] = base.State
			base.Labels["reason"] = err.Error()
			return base, nil
		}
		return StatusView{}, err
	}
	if err := validateRemoteOwnership(b.cfg, claim, job); err != nil {
		return StatusView{}, err
	}
	ready, err := b.currentAllocation(ctx, client, jobID)
	if err != nil {
		return StatusView{}, err
	}
	view := base
	view.State = ready.State()
	view.Ready = view.State == "running"
	view.Labels = baseStatusLabels(b.cfg, claim, view.State)
	applyReadinessLabels(view.Labels, ready)
	if wait && !view.Ready && isTerminalAllocationStatus(ready.ClientStatus) {
		view.State = "terminal"
		view.Labels[claimLabelState] = view.State
	}
	return view, nil
}

func (b *backend) waitForEvaluation(ctx context.Context, client Client, evalID string) error {
	timeout := b.evalTimeout()
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		eval, err := client.EvaluationInfo(pollCtx, evalID)
		if err != nil {
			return err
		}
		switch strings.TrimSpace(eval.Status) {
		case nomadapi.EvalStatusComplete:
			return nil
		case nomadapi.EvalStatusFailed, nomadapi.EvalStatusCancelled:
			return exit(5, "nomad evaluation %s ended with status=%s description=%s", evalID, eval.Status, eval.StatusDescription)
		}
		select {
		case <-pollCtx.Done():
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return exit(5, "timed out waiting for nomad evaluation %s", evalID)
			}
			return pollCtx.Err()
		case <-time.After(statusPollInterval):
		}
	}
}

func (b *backend) waitForAllocation(ctx context.Context, client Client, jobID string, timeout time.Duration) (allocationReadiness, error) {
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		ready, err := b.currentAllocation(pollCtx, client, jobID)
		if err != nil {
			return allocationReadiness{}, err
		}
		if ready.State() == "running" {
			return ready, nil
		}
		if ready.State() == "terminal" {
			return allocationReadiness{}, exit(5, "nomad job %s allocation %s reached terminal status client=%s desired=%s", jobID, ready.AllocationID, ready.ClientStatus, ready.DesiredStatus)
		}
		select {
		case <-pollCtx.Done():
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return allocationReadiness{}, exit(5, "timed out waiting for nomad job %s allocation readiness", jobID)
			}
			return allocationReadiness{}, pollCtx.Err()
		case <-time.After(statusPollInterval):
		}
	}
}

func (b *backend) currentAllocation(ctx context.Context, client Client, jobID string) (allocationReadiness, error) {
	allocs, err := client.JobAllocations(ctx, jobID, true)
	if err != nil {
		return allocationReadiness{}, err
	}
	return selectAllocation(allocs, jobID, b.cfg.Nomad.Task)
}

func selectAllocation(allocs []*nomadapi.AllocationListStub, jobID, taskName string) (allocationReadiness, error) {
	var fallback allocationReadiness
	for _, alloc := range allocs {
		if alloc == nil || alloc.JobID != jobID {
			continue
		}
		state, ok := alloc.TaskStates[taskName]
		if !ok {
			continue
		}
		ready := allocationReadiness{
			JobID:         jobID,
			AllocationID:  alloc.ID,
			Task:          taskName,
			NodeID:        alloc.NodeID,
			NodeName:      alloc.NodeName,
			ClientStatus:  alloc.ClientStatus,
			DesiredStatus: alloc.DesiredStatus,
			TaskState:     state.State,
		}
		if ready.State() == "running" && strings.EqualFold(state.State, "running") && !state.Failed {
			return ready, nil
		}
		if fallback.AllocationID == "" {
			fallback = ready
		}
	}
	if fallback.AllocationID != "" {
		return fallback, nil
	}
	return allocationReadiness{JobID: jobID, Task: taskName}, nil
}

func baseStatusLabels(cfg Config, claim LeaseClaim, state string) map[string]string {
	labels := map[string]string{
		"provider":          providerName,
		"lease":             claim.LeaseID,
		"slug":              claim.Slug,
		"target":            targetLinux,
		"pond":              claim.Pond,
		claimLabelJobID:     claim.Labels[claimLabelJobID],
		claimLabelTask:      cfg.Nomad.Task,
		claimLabelWorkdir:   cfg.Nomad.Workdir,
		claimLabelNamespace: normalizeNamespace(cfg.Nomad.Namespace),
		claimLabelRegion:    normalizeRegion(cfg.Nomad.Region),
		claimLabelState:     state,
	}
	if expiresAt := strings.TrimSpace(claim.Labels[claimLabelExpiresAt]); expiresAt != "" {
		labels[claimLabelExpiresAt] = expiresAt
	}
	return labels
}

func applyReadinessLabels(labels map[string]string, ready allocationReadiness) {
	labels[claimLabelAllocationID] = ready.AllocationID
	labels[claimLabelNodeID] = ready.NodeID
	labels[claimLabelNodeName] = ready.NodeName
	labels[claimLabelClientStatus] = ready.ClientStatus
	labels[claimLabelDesired] = ready.DesiredStatus
	if ready.TaskState != "" {
		labels["task_state"] = ready.TaskState
	}
}

func (b *backend) client() (Client, error) {
	if b.clientFactory != nil {
		return b.clientFactory(b.cfg, b.rt)
	}
	return newNomadClient(b.cfg, b.rt)
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

func (b *backend) allocReadyTimeout() time.Duration {
	if b.cfg.Nomad.AllocReadyTimeout > 0 {
		return b.cfg.Nomad.AllocReadyTimeout
	}
	return 5 * time.Minute
}

func (b *backend) evalTimeout() time.Duration {
	if b.cfg.Nomad.EvalTimeout > 0 {
		return b.cfg.Nomad.EvalTimeout
	}
	return 5 * time.Minute
}

func isTerminalAllocationStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case nomadapi.AllocClientStatusComplete, nomadapi.AllocClientStatusFailed, nomadapi.AllocClientStatusLost:
		return true
	default:
		return false
	}
}

func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	if classifyError(err) == "not_found" {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "404")
}
