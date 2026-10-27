package azuresandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) retentionPolicy() (*core.SessionDiskRetentionPolicy, error) {
	c := b.cfg.AzureSandbox
	if c.Retention == "" {
		if c.OwnerBinding != "" {
			return nil, fmt.Errorf("owner binding requires session-disk retention")
		}
		return nil, nil
	}
	if c.Retention != "session-disk" || !core.FixedSHA256(c.OwnerBinding) || b.cfg.TTL < time.Second || b.cfg.TTL%time.Second != 0 {
		return nil, fmt.Errorf("session-disk retention requires SHA256 owner binding and finite positive whole-second TTL")
	}
	return &core.SessionDiskRetentionPolicy{Mode: c.Retention, OwnerBinding: c.OwnerBinding, ComputeWindowSeconds: int(b.cfg.TTL / time.Second)}, nil
}

func (b *backend) verifyRetentionPolicy(claim core.LeaseClaim, s sandbox) error {
	p := s.Lifecycle
	if !s.policyObserved || p.AutoDelete.Enabled || !p.AutoSuspend.Enabled || p.AutoSuspend.Mode != "Disk" || p.AutoSuspend.Interval != claim.IdleTimeoutSeconds {
		return fmt.Errorf("retained sandbox requires independent explicit no-auto-delete and Disk idle policy readback")
	}
	return nil
}

func normalizedRetentionState(s sandbox) (string, error) {
	if s.StateDetails.StoppedReason == "Disabled" {
		return "", fmt.Errorf("retained sandbox is administratively disabled")
	}
	switch s.State {
	case "Running":
		return "running", nil
	case "Stopped", "Suspended", "Idle":
		return "suspended", nil
	case "Creating", "Starting", "Resuming", "Stopping", "Suspending":
		return "transitioning", nil
	default:
		return "", fmt.Errorf("retained sandbox state is unknown")
	}
}

func (b *backend) Pause(ctx context.Context, req core.PauseRequest) error {
	return b.retentionLifecycle(ctx, req.ID, req.Lifecycle, "pause")
}
func (b *backend) Resume(ctx context.Context, req core.ResumeRequest) error {
	return b.retentionLifecycle(ctx, req.ID, req.Lifecycle, "resume")
}

func (b *backend) retentionLifecycle(ctx context.Context, id string, req core.RetentionLifecycleRequest, action string) error {
	claim, err := b.claim(id)
	if err != nil {
		return err
	}
	if claim.FixedCreateIntent.Retention == nil || id != claim.LeaseID {
		return fmt.Errorf("Azure pause/resume requires a canonical retained session lease")
	}
	if err := core.ValidateRetentionLifecycleRequest(claim, req, action); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	// Reuse activity interruption, but the durable lifecycle intent, not this
	// transient signal, remains authority across a crash.
	op := claim.FixedCreateIntent.Journal.Retention.Operation
	observedResumeReplay := action == "resume" && op != nil && op.ID == req.OperationID && op.Phase == "observed"
	if !observedResumeReplay {
		release, err := b.interruptActivity(ctx, claim, "")
		if err != nil {
			return err
		}
		defer release()
	}
	return core.MutateFixedRetention(ctx, leaseKind, claim, req, action, b.now, func(ctx context.Context, c core.LeaseClaim) (string, error) {
		s, err := b.inspect(ctx, c)
		if err != nil {
			return "", err
		}
		if err := b.verifyRetentionPolicy(c, s); err != nil {
			return "", err
		}
		return normalizedRetentionState(s)
	}, func(ctx context.Context, c core.LeaseClaim, target string) error {
		if target == "suspended" {
			return b.api.Pause(ctx, c.CloudID)
		}
		return b.api.Resume(ctx, c.CloudID)
	})
}

func (b *backend) retainedStatus(ctx context.Context, claim core.LeaseClaim, view core.StatusView) (core.StatusView, error) {
	i := claim.FixedCreateIntent
	if err := core.ValidateSessionDiskRetentionClaim(claim); err != nil {
		return view, err
	}
	state := *i.Journal.Retention
	view.Retention = &core.SessionDiskRetentionView{SessionDiskRetentionPolicy: *i.Retention, SessionDiskRetentionState: state, Schema: "crabbox.session-disk.v1", ClaimRevision: claim.Revision, Identity: core.RetainedProviderIdentity{LeaseID: claim.LeaseID, AttemptLeaseID: claim.LeaseID, Slug: claim.Slug, ResourceID: claim.CloudID}}
	view.ExpiresAt = state.ComputeWindow.ExpiresAt
	if i.State == "released" {
		view.State = "deleted"
		return view, nil
	}
	if i.Journal.Phase == "deleting" {
		view.Retention.DesiredState = "retired"
	}
	observed, err := core.InspectFixedResource(ctx, leaseKind, claim, core.FixedLeaseOperations[sandbox]{ObserveExact: b.observe})
	if err != nil {
		if errors.Is(err, errSandboxOwnership) {
			return view, err
		}
		view.State = "unknown"
		end, _ := time.Parse(time.RFC3339Nano, state.ComputeWindow.ExpiresAt)
		view.Retention.StopPending = state.DesiredState != "running" || state.ComputeWindow.State == "closed" || !end.After(b.now())
		return view, nil
	}
	if len(observed.Candidates) == 0 {
		view.State = "missing"
		if claim.CloudID == "" {
			view.State = "unknown"
		}
		return view, nil
	}
	s := observed.Candidates[0]
	view.ServerID = s.ID
	view.Labels = s.Labels
	view.Retention.ObservedState = s.State
	view.Retention.PolicyVerified = b.verifyRetentionPolicy(claim, s) == nil
	normalized, err := normalizedRetentionState(s)
	if err != nil {
		view.State = "unknown"
		return view, nil
	}
	view.State = normalized
	_, admissionErr := core.SessionDiskExecutionDeadline(claim, b.now())
	pending := state.Operation != nil && state.Operation.Phase == "pending"
	end, _ := time.Parse(time.RFC3339Nano, state.ComputeWindow.ExpiresAt)
	view.Retention.StopPending = (state.DesiredState != "running" || state.ComputeWindow.State == "closed" || !end.After(b.now())) && normalized != "suspended"
	if i.State != "acquired" {
		view.State = "transitioning"
	} else if i.Journal.Phase == "deleting" {
		view.State = "transitioning"
		view.Retention.StopPending = true
	} else if pending {
		view.State = "transitioning"
	} else if !view.Retention.PolicyVerified {
		view.State = "unknown"
	} else if state.ComputeWindow.State == "active" && admissionErr != nil {
		view.State = "compute-expired"
	}
	view.Ready = view.State == "running" && admissionErr == nil && view.Retention.PolicyVerified
	return view, nil
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}
func (b *backend) executionDeadline(claim core.LeaseClaim) (time.Time, error) {
	if claim.FixedCreateIntent != nil && claim.FixedCreateIntent.Retention != nil {
		return core.SessionDiskExecutionDeadline(claim, b.now())
	}
	return executionDeadline(claim)
}
