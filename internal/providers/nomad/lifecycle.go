package nomad

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
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
	TaskFailed    bool
}

func (r allocationReadiness) State() string {
	if r.AllocationID == "" {
		return "not-ready"
	}
	taskState := strings.ToLower(strings.TrimSpace(r.TaskState))
	if r.ClientStatus == nomadapi.AllocClientStatusRunning && r.DesiredStatus == nomadapi.AllocDesiredStatusRun && taskState == "running" && !r.TaskFailed {
		return "running"
	}
	if isTerminalAllocationStatus(r.ClientStatus) || r.DesiredStatus != "" && r.DesiredStatus != nomadapi.AllocDesiredStatusRun || r.TaskFailed || isTerminalTaskState(taskState) {
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
	leaseID, slug, ready, _, err := b.createJob(ctx, client, req.Repo, req.RequestedSlug, req.Reclaim)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s job=%s allocation=%s task=%s workdir=%s\n", leaseID, slug, providerName, ready.JobID, ready.AllocationID, b.cfg.Nomad.Task, b.cfg.Nomad.Workdir)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: nomad warmup keeps the job until explicit stop\n")
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
			Workdir:  b.cfg.Nomad.Workdir,
		})
	}
	return nil
}

func (b *backend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	req.ID = strings.TrimSpace(req.ID)
	workdir := strings.TrimSpace(b.cfg.Nomad.Workdir)
	var client Client
	var claim LeaseClaim
	var ready allocationReadiness
	bound := func() shared.DelegatedSandbox {
		return shared.DelegatedSandbox{
			LeaseID: claim.LeaseID, Slug: claim.Slug,
			CleanupCommand: "crabbox stop --provider nomad " + shellQuote(claim.LeaseID),
		}
	}
	return shared.RunDelegatedSandbox(ctx, req, shared.DelegatedSandboxLifecycle{
		Provider: providerName, Runtime: b.rt, Workdir: workdir,
		IdleTimeout: b.cfg.IdleTimeout, TTL: b.cfg.TTL,
		Preflight: func(context.Context) error {
			if req.Options.Tailscale.Enabled {
				return exit(2, "provider=%s is delegated-run only and does not support Tailscale options", providerName)
			}
			if err := delegatedSyncOptionsError(b.spec, req); err != nil {
				return err
			}
			if !req.SyncOnly && (len(req.Command) == 0 || len(req.Command) == 1 && strings.TrimSpace(req.Command[0]) == "") {
				return exit(2, "missing command")
			}
			var err error
			client, err = b.client()
			return err
		},
		PrepareArchive: func(ctx context.Context) (*core.PreparedArchive, error) {
			return core.PrepareDelegatedArchive(ctx, core.DelegatedArchivePreparationRequest{
				Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
				TempPattern: "crabbox-nomad-sync-*.tgz", Stderr: b.rt.Stderr, Now: b.now,
			})
		},
		Acquire: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			_, _, ready, claim, err = b.createJob(ctx, client, req.Repo, req.RequestedSlug, req.Reclaim)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			fmt.Fprintf(b.rt.Stderr, "leased %s slug=%s provider=%s job=%s allocation=%s task=%s\n", claim.LeaseID, claim.Slug, providerName, ready.JobID, ready.AllocationID, ready.Task)
			return bound(), nil
		},
		Resolve: func(ctx context.Context) (shared.DelegatedSandbox, error) {
			var err error
			claim, err = resolveNomadClaim(b.cfg, req.ID)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			jobID := claim.Labels[claimLabelJobID]
			job, err := client.JobInfo(ctx, jobID)
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			if err := validateRemoteOwnership(b.cfg, claim, job); err != nil {
				return shared.DelegatedSandbox{}, err
			}
			claimedWorkdir := strings.TrimSpace(claim.Labels[claimLabelWorkdir])
			if claimedWorkdir != "" && claimedWorkdir != workdir {
				return shared.DelegatedSandbox{}, exit(2, "nomad lease %s uses workdir %s; requested workdir %s differs; stop the lease or rerun with the matching --nomad-workdir", claim.LeaseID, claimedWorkdir, workdir)
			}
			ready, err = b.waitForAllocation(ctx, client, jobID, b.allocReadyTimeout())
			if err != nil {
				return shared.DelegatedSandbox{}, err
			}
			if req.Repo.Root != "" {
				if err := ctx.Err(); err != nil {
					return shared.DelegatedSandbox{}, err
				}
				updated, err := core.ClaimLeaseForRepoProviderScopePondIfUnchanged(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, b.cfg.Pond, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim, claim, true)
				if err != nil {
					return shared.DelegatedSandbox{}, err
				}
				claim, err = updateLeaseClaimLabelsIfUnchanged(claim.LeaseID, updated, claimLabels(b.cfg, claim.LeaseID, claim.Slug, ready, claimExpiresAt(claim)))
				if err != nil {
					return shared.DelegatedSandbox{}, err
				}
			}
			return bound(), nil
		},
		Setup: func(context.Context) error {
			fmt.Fprintf(b.rt.Stderr, "provider=%s lease=%s job=%s allocation=%s task=%s workdir=%s\n", providerName, claim.LeaseID, ready.JobID, ready.AllocationID, ready.Task, workdir)
			return nil
		},
		Sync: func(ctx context.Context, prepared *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
			return b.syncWorkspace(ctx, client, ready, req, workdir, prepared)
		},
		NoSync: func(ctx context.Context) error {
			return b.execShell(ctx, client, ready, "mkdir -p "+shellQuote(workdir))
		},
		Command: func(context.Context) (shared.DelegatedSandboxCommand, error) {
			return shared.DelegatedSandboxCommand{
				Text: strings.Join(req.Command, " "),
				Run: func(ctx context.Context) (int, error) {
					return b.runCommand(ctx, client, ready, req, workdir)
				},
			}, nil
		},
		Retained: func(context.Context) error {
			return refreshNomadLeaseActivity(b.cfg, claim)
		},
		Cleanup: func(ctx context.Context) error {
			if err := b.deleteOwnedRunJob(ctx, client, claim); err != nil {
				return fmt.Errorf("nomad stop failed for lease=%s job=%s: %w", claim.LeaseID, claim.Labels[claimLabelJobID], err)
			}
			return nil
		},
	})
}

func (b *backend) createJob(ctx context.Context, client Client, repo Repo, requestedSlug string, reclaim bool) (string, string, allocationReadiness, LeaseClaim, error) {
	leaseID, err := newLeaseID()
	if err != nil {
		return "", "", allocationReadiness{}, LeaseClaim{}, err
	}
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
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
			return "", "", allocationReadiness{}, LeaseClaim{}, b.cleanupUnclaimedJob(ctx, client, job, err)
		}
	}
	ready, err := b.waitForAllocation(ctx, client, jobID, b.allocReadyTimeout())
	if err != nil {
		return "", "", allocationReadiness{}, LeaseClaim{}, b.cleanupUnclaimedJob(ctx, client, job, err)
	}
	claim, err := writeNomadClaim(b.cfg, leaseID, slug, repo, reclaim, ready, expiresAt)
	if err != nil {
		return "", "", allocationReadiness{}, LeaseClaim{}, b.cleanupUnclaimedJob(ctx, client, job, err)
	}
	return leaseID, slug, ready, claim, nil
}

func (b *backend) cleanupUnclaimedJob(ctx context.Context, client Client, expected *nomadapi.Job, cause error) error {
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	jobID := stringValue(expected.ID)
	leaseID := expected.Meta[metadataLeaseID]
	cleanupErr := core.CleanupLeaseClaimIfUnchangedAfter(leaseID, LeaseClaim{}, false, func() error {
		if err := cleanupCtx.Err(); err != nil {
			return err
		}
		job, err := client.JobInfo(cleanupCtx, jobID)
		if err != nil {
			if isNotFoundError(err) {
				return nil
			}
			return fmt.Errorf("inspect nomad job %s before setup cleanup: %w", jobID, err)
		}
		if job == nil || stringValue(job.ID) != jobID || !metadataMatches(job.Meta, expected.Meta) {
			return exit(4, "refusing cleanup of nomad job %s after setup failure: ownership changed", jobID)
		}
		return b.deregisterJobAndConfirmAbsent(cleanupCtx, client, jobID)
	})
	if cleanupErr != nil {
		return errors.Join(cause, fmt.Errorf("cleanup nomad job %s after unclaimed setup failure: %w", jobID, cleanupErr))
	}
	return cause
}

func (b *backend) deregisterJobAndConfirmAbsent(ctx context.Context, client Client, jobID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	evalID, err := client.DeregisterJob(ctx, jobID, true)
	if err != nil {
		return err
	}
	if evalID != "" {
		if err := b.waitForEvaluation(ctx, client, evalID); err != nil {
			return fmt.Errorf("wait for nomad deregistration evaluation %s: %w", evalID, err)
		}
	}
	for {
		job, err := client.JobInfo(ctx, jobID)
		if isNotFoundError(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("confirm nomad job %s removal: %w", jobID, err)
		}
		if job == nil {
			return fmt.Errorf("confirm nomad job %s removal: empty response", jobID)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("confirm nomad job %s removal: %w", jobID, ctx.Err())
		case <-time.After(statusPollInterval):
		}
	}
}

func (b *backend) deleteOwnedRunJob(ctx context.Context, client Client, expected LeaseClaim) error {
	if expected.LeaseID == "" {
		return nil
	}
	_, err := b.removeOwnedJob(ctx, client, expected, false)
	return err
}

// The original claim is the authority, including for a run that finishes after
// another caller changed it. Keep the fence through remote absence and removal.
func (b *backend) removeOwnedJob(ctx context.Context, client Client, expected LeaseClaim, requireMissing bool) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, b.evalTimeout())
	defer cancel()
	missing := false
	err := core.RemoveLeaseClaimIfUnchangedAfter(expected.LeaseID, expected, func() error {
		// Lock admission is not cancelable; an expired waiter must do no work.
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := authorizeClaimScope(b.cfg, expected); err != nil {
			return err
		}
		jobID := expected.Labels[claimLabelJobID]
		if strings.TrimSpace(jobID) == "" {
			return exit(4, "nomad lease %s has no job ID", expected.LeaseID)
		}
		job, err := client.JobInfo(ctx, jobID)
		if err != nil {
			if isNotFoundError(err) {
				missing = true
				return nil
			}
			return err
		}
		if requireMissing {
			return exit(4, "refusing removal of nomad claim %s: job %s reappeared", expected.LeaseID, jobID)
		}
		if err := validateRemoteOwnership(b.cfg, expected, job); err != nil {
			return err
		}
		return b.deregisterJobAndConfirmAbsent(ctx, client, jobID)
	})
	return missing, err
}

func refreshNomadLeaseActivity(cfg Config, claim LeaseClaim) error {
	if claim.LeaseID == "" {
		return nil
	}
	idleTimeout := cfg.IdleTimeout
	if idleTimeout <= 0 && claim.IdleTimeoutSeconds > 0 {
		idleTimeout = time.Duration(claim.IdleTimeoutSeconds) * time.Second
	}
	// Refresh only the captured claim; rereading after an unguarded claim write
	// could recreate a retired lease or overwrite a successor's job identity.
	_, err := core.ClaimLeaseForRepoProviderScopePondIfUnchanged(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, claim.Pond, claim.RepoRoot, idleTimeout, false, claim, true)
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
		if view.State == "terminal" || view.State == "missing" {
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
	missing, err := b.removeOwnedJob(ctx, client, claim, false)
	if err != nil {
		return err
	}
	if missing {
		fmt.Fprintf(b.rt.Stderr, "removed stale nomad claim lease=%s job=%s reason=missing\n", claim.LeaseID, jobID)
		return nil
	}
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
		jobID := claim.Labels[claimLabelJobID]
		job, err := client.JobInfo(ctx, jobID)
		if err != nil {
			if !isNotFoundError(err) {
				return err
			}
			if req.DryRun {
				if err := core.VerifyLeaseClaimUnchanged(claim.LeaseID, claim); err != nil {
					return err
				}
				fmt.Fprintf(b.rt.Stdout, "would remove nomad claim lease=%s job=%s reason=missing\n", claim.LeaseID, jobID)
				continue
			}
			if _, err := b.removeOwnedJob(ctx, client, claim, true); err != nil {
				return err
			}
			fmt.Fprintf(b.rt.Stdout, "remove nomad claim lease=%s job=%s reason=missing\n", claim.LeaseID, jobID)
			removed++
			continue
		}
		due, reason := claimCleanupDue(claim, now)
		if !due {
			fmt.Fprintf(b.rt.Stderr, "skip nomad job=%s lease=%s reason=%s\n", jobID, claim.LeaseID, reason)
			continue
		}
		if err := validateRemoteOwnership(b.cfg, claim, job); err != nil {
			return err
		}
		if req.DryRun {
			if err := core.VerifyLeaseClaimUnchanged(claim.LeaseID, claim); err != nil {
				return err
			}
			fmt.Fprintf(b.rt.Stdout, "would deregister nomad job=%s lease=%s reason=%s\n", jobID, claim.LeaseID, reason)
			continue
		}
		if _, err := b.removeOwnedJob(ctx, client, claim, false); err != nil {
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
			base.State = "missing"
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
	result, err := shared.Poll(context.WithoutCancel(pollCtx), 0, statusPollInterval,
		func(context.Context, time.Duration) error {
			if err := shared.SleepContext(pollCtx, statusPollInterval); err != nil {
				return pollCtx.Err()
			}
			return nil
		},
		func(context.Context) (*nomadapi.Evaluation, error) { return client.EvaluationInfo(pollCtx, evalID) },
		func(_ context.Context, eval *nomadapi.Evaluation, fetchErr error) (bool, error) {
			if fetchErr != nil {
				return false, fetchErr
			}
			if eval == nil {
				return false, exit(5, "nomad evaluation %s returned no data", evalID)
			}
			switch strings.TrimSpace(eval.Status) {
			case nomadapi.EvalStatusComplete:
				return true, nil
			case nomadapi.EvalStatusFailed, nomadapi.EvalStatusCancelled:
				return false, exit(5, "nomad evaluation %s ended with status=%s description=%s", evalID, eval.Status, eval.StatusDescription)
			}
			return false, nil
		}, nil)
	if err != nil && result.Err == nil && errors.Is(context.Cause(pollCtx), context.DeadlineExceeded) && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return exit(5, "timed out waiting for nomad evaluation %s", evalID)
	}
	return err
}

func (b *backend) waitForAllocation(ctx context.Context, client Client, jobID string, timeout time.Duration) (allocationReadiness, error) {
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := shared.Poll(context.WithoutCancel(pollCtx), 0, statusPollInterval,
		func(context.Context, time.Duration) error {
			if err := shared.SleepContext(pollCtx, statusPollInterval); err != nil {
				return pollCtx.Err()
			}
			return nil
		},
		func(context.Context) (allocationReadiness, error) { return b.currentAllocation(pollCtx, client, jobID) },
		func(_ context.Context, ready allocationReadiness, fetchErr error) (bool, error) {
			if fetchErr != nil {
				return false, fetchErr
			}
			if ready.State() == "running" {
				return true, nil
			}
			if ready.State() == "terminal" {
				return false, exit(5, "nomad job %s allocation %s reached terminal status client=%s desired=%s", jobID, ready.AllocationID, ready.ClientStatus, ready.DesiredStatus)
			}
			return false, nil
		}, nil)
	if err != nil {
		if result.Err == nil && errors.Is(context.Cause(pollCtx), context.DeadlineExceeded) && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return allocationReadiness{}, exit(5, "timed out waiting for nomad job %s allocation readiness", jobID)
		}
		return allocationReadiness{}, err
	}
	return result.Value, nil
}

func (b *backend) currentAllocation(ctx context.Context, client Client, jobID string) (allocationReadiness, error) {
	allocs, err := client.JobAllocations(ctx, jobID, true)
	if err != nil {
		return allocationReadiness{}, err
	}
	return selectAllocation(allocs, jobID, b.cfg.Nomad.Task)
}

func selectAllocation(allocs []*nomadapi.AllocationListStub, jobID, taskName string) (allocationReadiness, error) {
	var nonTerminal allocationReadiness
	var terminal allocationReadiness
	for _, alloc := range allocs {
		if alloc == nil || alloc.JobID != jobID {
			continue
		}
		state, ok := alloc.TaskStates[taskName]
		if !ok || state == nil {
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
			TaskFailed:    state.Failed,
		}
		if ready.State() == "running" {
			return ready, nil
		}
		if ready.State() == "terminal" {
			if terminal.AllocationID == "" {
				terminal = ready
			}
			continue
		}
		if nonTerminal.AllocationID == "" {
			nonTerminal = ready
		}
	}
	if nonTerminal.AllocationID != "" {
		return nonTerminal, nil
	}
	if terminal.AllocationID != "" {
		return terminal, nil
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

func isTerminalTaskState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "dead", "failed":
		return true
	default:
		return false
	}
}

func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	var statusErr interface{ StatusCode() int }
	return errors.As(err, &statusErr) && statusErr.StatusCode() == http.StatusNotFound
}
