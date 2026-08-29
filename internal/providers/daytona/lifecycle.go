package daytona

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	daytona "github.com/daytonaio/daytona/libs/api-client-go"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const daytonaActivityRequestTimeout = 10 * time.Second

func (b *daytonaLeaseBackend) createDaytonaSandbox(ctx context.Context, repo Repo, keep, reclaim bool, requestedSlug string) (sandbox *daytona.Sandbox, leaseID, slug string, err error) {
	if strings.TrimSpace(b.cfg.Daytona.Snapshot) == "" {
		return nil, "", "", exit(2, "provider=daytona requires --daytona-snapshot or daytona.snapshot")
	}
	if b.cfg.TTL <= 0 || b.cfg.IdleTimeout <= 0 || durationMinutesCeil(b.cfg.TTL) > math.MaxInt32 || durationMinutesCeil(b.cfg.IdleTimeout) > math.MaxInt32 {
		return nil, "", "", exit(2, "provider=daytona requires positive TTL and idle timeout within Daytona's minute range")
	}
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return nil, "", "", err
	}
	existing, err := client.ListCrabboxSandboxes(ctx)
	if err != nil {
		return nil, "", "", daytonaError("list sandboxes", err)
	}
	leaseID = newLeaseID()
	slug, err = allocateDirectLeaseSlug(leaseID, requestedSlug, daytonaSandboxesToServers(existing, b.cfg))
	if err != nil {
		return nil, "", "", err
	}
	cfg := b.cfg
	cfg.ServerType, cfg.WorkRoot, cfg.SSHUser, cfg.SSHPort = "snapshot", daytonaWorkRoot(cfg), daytonaUser(cfg), "22"
	labels := directLeaseLabels(cfg, leaseID, slug, daytonaProvider, "", keep, time.Now().UTC())
	labels["lease_name"], labels["work_root"] = leaseProviderName(leaseID, slug), cfg.WorkRoot
	body := daytona.NewCreateSandbox()
	body.SetName(labels["lease_name"])
	body.SetSnapshot(strings.TrimSpace(cfg.Daytona.Snapshot))
	body.SetUser(cfg.SSHUser)
	body.SetLabels(labels)
	body.SetPublic(false)
	body.SetAutoStopInterval(int32(durationMinutesCeil(cfg.IdleTimeout)))
	body.SetAutoDeleteInterval(-1)
	// The pinned generated client preserves newer API fields in AdditionalProperties.
	body.AdditionalProperties = map[string]interface{}{"ttlMinutes": durationMinutesCeil(cfg.TTL)}
	if target := strings.TrimSpace(cfg.Daytona.Target); target != "" {
		body.SetTarget(target)
	}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=daytona lease=%s slug=%s snapshot=%s target=%s keep=%v\n", leaseID, slug, cfg.Daytona.Snapshot, blank(cfg.Daytona.Target, "-"), keep)
	created, createErr := client.CreateSandbox(ctx, *body)
	if createErr != nil || created == nil || created.GetId() == "" {
		if createErr == nil {
			createErr = errors.New("create response missing sandbox id")
		}
		// Never retry allocation after an ambiguous response; recover only this attempt.
		recoveryCtx, cancel := daytonaCleanupContext()
		defer cancel()
		var recoveryErr error
		created, recoveryErr = recoverDaytonaAllocation(recoveryCtx, client, labels["lease_name"], leaseID)
		if recoveryErr != nil {
			return nil, leaseID, slug, fmt.Errorf("%w; cannot reconcile Daytona lease %s: %v", daytonaError("create sandbox", createErr), leaseID, recoveryErr)
		}
	}
	resourceID := created.GetId()
	defer func() {
		if err != nil {
			err = b.rollbackDaytonaSandbox(resourceID, leaseID, err)
		}
	}()
	if err = claimLeaseTargetForRepoConfig(leaseID, slug, cfg, Server{Provider: daytonaProvider, CloudID: resourceID, Labels: labels}, SSHTarget{}, repo.Root, cfg.IdleTimeout, reclaim); err != nil {
		return nil, leaseID, slug, err
	}
	if createErr != nil {
		return nil, leaseID, slug, daytonaError("create sandbox", createErr)
	}
	sandbox, err = waitForDaytonaReady(ctx, client, resourceID, 5*time.Minute)
	if err != nil {
		return nil, leaseID, slug, err
	}
	labels["state"], labels["last_touched_at"] = "ready", leaseLabelTime(time.Now().UTC())
	sandbox, err = establishDaytonaSandboxOwnership(ctx, client, resourceID, leaseID, labels)
	return sandbox, leaseID, slug, err
}

func recoverDaytonaAllocation(ctx context.Context, client daytonaAPI, name, leaseID string) (*daytona.Sandbox, error) {
	for {
		// Inventory is eventually consistent. Resolve the unique name, but never
		// treat a matching name alone as proof that this attempt owns the resource.
		sandbox, err := client.GetSandbox(ctx, name)
		if err == nil && sandbox != nil && sandbox.GetId() != "" {
			if id, owned := daytonaSandboxOwnership(sandbox); !owned || id != leaseID || sandbox.GetName() != name {
				return nil, fmt.Errorf("sandbox named %s does not match ownership of lease %s; refusing recovery", name, leaseID)
			}
			return sandbox, nil
		}
		if waitErr := shared.SleepContext(ctx, time.Second); waitErr != nil {
			return nil, fmt.Errorf("allocation unconfirmed for sandbox name=%s: %w; inspect provider inventory before retrying", name, waitErr)
		}
	}
}

func (b *daytonaLeaseBackend) rollbackDaytonaSandbox(resourceID, leaseID string, cause error) error {
	ctx, cancel := daytonaCleanupContext()
	defer cancel()
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err == nil {
		err = deleteOwnedDaytonaSandbox(ctx, client, resourceID, leaseID)
	}
	if err != nil {
		return fmt.Errorf("%w; cleanup failed for Daytona lease=%s sandbox=%s: %v; retry crabbox stop --provider daytona %s", cause, leaseID, resourceID, err, leaseID)
	}
	removeLeaseClaim(leaseID)
	return cause
}

func deleteOwnedDaytonaSandbox(ctx context.Context, client daytonaAPI, resourceID, leaseID string) error {
	sandbox, err := client.GetSandbox(ctx, resourceID)
	if daytonaIsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return daytonaError("verify sandbox before deletion", err)
	}
	if id, owned := daytonaSandboxOwnership(sandbox); !owned || id != leaseID || sandbox.GetId() != resourceID {
		return exit(4, "refusing to delete Daytona sandbox %s: ownership does not match lease %s", resourceID, leaseID)
	}
	if daytonaStateDeleted(daytonaSandboxState(sandbox)) {
		return nil
	}
	// A retry should wait for an accepted deletion, not submit another DELETE
	// that Daytona rejects with a conflict while destruction is in progress.
	state := daytonaSandboxState(sandbox)
	if state != "destroying" && state != "deleting" {
		if err := client.DeleteSandbox(ctx, resourceID); err != nil && !daytonaIsNotFoundError(err) {
			return daytonaError("delete sandbox", err)
		}
	}
	for {
		sandbox, err = client.GetSandbox(ctx, resourceID)
		if daytonaIsNotFoundError(err) || err == nil && daytonaStateDeleted(daytonaSandboxState(sandbox)) {
			return nil
		}
		if err != nil {
			return daytonaError("confirm sandbox deletion", err)
		}
		if err := shared.SleepContext(ctx, time.Second); err != nil {
			return err
		}
	}
}

func daytonaStateDeleted(state string) bool {
	return state == "destroyed" || state == "deleted"
}

func daytonaActivityInterval(idle time.Duration) time.Duration {
	interval := idle / 3
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	if interval < time.Second {
		interval = time.Second
	}
	return interval
}

func (b *daytonaLeaseBackend) startDaytonaActivity(ctx context.Context, sandbox *daytona.Sandbox) (func(), error) {
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return nil, err
	}
	refresh := func(callCtx context.Context) error {
		callCtx, cancel := context.WithTimeout(callCtx, daytonaActivityRequestTimeout)
		defer cancel()
		return client.UpdateLastActivity(callCtx, sandbox.GetId())
	}
	if err := refresh(ctx); err != nil {
		return nil, daytonaError("refresh activity", err)
	}
	idle := time.Duration(sandbox.GetAutoStopInterval()) * time.Minute
	if idle <= 0 {
		idle = b.cfg.IdleTimeout
	}
	activityCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(daytonaActivityInterval(idle))
		defer ticker.Stop()
		for {
			select {
			case <-activityCtx.Done():
				return
			case <-ticker.C:
				if err := refresh(activityCtx); err != nil && activityCtx.Err() == nil {
					fmt.Fprintf(b.rt.Stderr, "warning: daytona activity refresh failed: %v\n", daytonaError("refresh activity", err))
				}
			}
		}
	}()
	return func() { cancel(); <-done }, nil
}

func daytonaTouchedLabels(labels map[string]string, cfg Config, req TouchRequest) map[string]string {
	if req.IdleTimeoutOverride != nil {
		rounded := time.Duration(durationMinutesCeil(*req.IdleTimeoutOverride)) * time.Minute
		req.IdleTimeoutOverride = &rounded
	}
	return core.TouchDirectLeaseLabelsWithIdleTimeoutOverride(labels, cfg, req.State, time.Now().UTC(), req.IdleTimeoutOverride)
}
