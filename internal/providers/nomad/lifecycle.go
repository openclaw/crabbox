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

func (b *backend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{Provider: providerName}, exit(2, "nomad run is deferred to the allocation exec/archive sync wave")
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
