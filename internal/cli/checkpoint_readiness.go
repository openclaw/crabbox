package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// Invocation-only policy: the existing fixed intent and lifecycle owners retain
// custody. This adds no durable state and never treats an observation as ready.
type checkpointReadinessKey struct{}

type checkpointReadinessWait struct {
	Timeout               time.Duration
	CheckpointID, LeaseID string
	LifetimeDeadline      time.Time
}

func checkpointReadinessFromContext(ctx context.Context) (checkpointReadinessWait, bool) {
	p, ok := ctx.Value(checkpointReadinessKey{}).(checkpointReadinessWait)
	return p, ok
}

// WithCheckpointReadinessLifetime supplies recorded lifetime metadata to the
// existing bootstrap wait. It is inert outside a fixed checkpoint invocation.
func WithCheckpointReadinessLifetime(ctx context.Context, server Server) context.Context {
	p, ok := checkpointReadinessFromContext(ctx)
	if !ok {
		return ctx
	}
	p.LifetimeDeadline, _ = parseLeaseLabelTime(server.Labels["expires_at"])
	if policy := server.LifetimePolicy; policy != nil && policy.Mode == "enforce" {
		stop := firstNonBlank(policy.EffectiveStopAt, policy.DeadlineAt)
		if policy.EffectiveStopAt == "" && policy.Extension != nil {
			base, _ := time.Parse(time.RFC3339Nano, stop)
			if extension, err := time.Parse(time.RFC3339Nano, policy.Extension.ExpiresAt); err == nil && extension.After(base) {
				stop = policy.Extension.ExpiresAt
			}
		}
		if deadline, err := time.Parse(time.RFC3339Nano, stop); err == nil && (p.LifetimeDeadline.IsZero() || deadline.Before(p.LifetimeDeadline)) {
			p.LifetimeDeadline = deadline
		}
	}
	return context.WithValue(ctx, checkpointReadinessKey{}, p)
}

func (p checkpointReadinessWait) lifetimeSummary() string {
	if p.LifetimeDeadline.IsZero() {
		return ""
	}
	return " observed_lease_deadline=" + p.LifetimeDeadline.UTC().Format(time.RFC3339)
}

func (p checkpointReadinessWait) failure(state, phase string, elapsed time.Duration, deadline time.Time) error {
	next := fmt.Sprintf("continue waiting by repeating the same checkpoint fork with --lease-id %s and the same lease options", p.LeaseID)
	if state == "bootstrap_failed" {
		next = fmt.Sprintf("inspect the failed bootstrap phase before replaying --lease-id %s", p.LeaseID)
	}
	return Exit(5, "checkpoint readiness lease=%s checkpoint=%s state=%s phase=%s elapsed=%s wait_deadline=%s%s; lease identity retained; %s; no replacement was allocated", p.LeaseID, p.CheckpointID, state, phase, elapsed.Round(time.Second), deadline.UTC().Format(time.RFC3339), p.lifetimeSummary(), next)
}

// Read only a closed vocabulary, never guest logs, arbitrary process arguments,
// or stderr. A successful observation is still not a successful ready check.
func observeReadinessStatus(ctx context.Context, target SSHTarget) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	target.NoControlMaster = true
	out := newSynchronizedBuffer(256)
	err := executeSSH(ctx, &target, target.ReadinessStatus, nil, 0, 0, "5", "1", &out, io.Discard)
	if err != nil {
		return "", ""
	}
	fields := strings.Fields(out.String())
	if len(fields) != 2 || (fields[0] != "pending" && fields[0] != "failed") {
		return "", ""
	}
	switch fields[1] {
	case "bootstrap", "ownership-repair", "cloud-config", "cloud-final", "workspace-ready", "readiness-check", "unknown":
		return fields[0], fields[1]
	}
	return "", ""
}

func checkpointReadinessLifetimeError(ctx context.Context, deadline time.Time) error {
	p, ok := checkpointReadinessFromContext(ctx)
	if !ok || deadline.IsZero() || time.Now().Before(deadline) {
		return nil
	}
	return Exit(5, "checkpoint readiness lease=%s state=lifetime_exhausted lease_deadline=%s; inspect or explicitly extend the existing lease; no replacement was allocated", p.LeaseID, deadline.UTC().Format(time.RFC3339))
}

// CheckCheckpointReadinessLifetime checks only recorded policy, without extending
// it. The provider's existing monitor still owns live identity/state checks.
func CheckCheckpointReadinessLifetime(ctx context.Context, server Server) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	ctx = WithCheckpointReadinessLifetime(ctx, server)
	p, _ := checkpointReadinessFromContext(ctx)
	return checkpointReadinessLifetimeError(ctx, p.LifetimeDeadline)
}
