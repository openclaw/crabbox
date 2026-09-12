package shared

import (
	"context"
	"errors"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// StatusWait bounds provider requests as well as the delay between polls.
// Adapters retain resolution, readiness, terminal-state, and API-error policy.
type StatusWait struct {
	parent   context.Context
	ctx      context.Context
	cancel   context.CancelFunc
	clock    core.Clock
	deadline time.Time
	timeout  func(string) error
}

func NewStatusWait(ctx context.Context, req core.StatusRequest, clock core.Clock, timeout func(string) error) *StatusWait {
	duration := req.WaitTimeout
	if duration <= 0 {
		duration = 5 * time.Minute
	}
	wait := &StatusWait{
		parent:   ctx,
		ctx:      ctx,
		cancel:   func() {},
		clock:    clock,
		deadline: core.ClockNow(clock).Add(duration),
		timeout:  timeout,
	}
	if req.Wait {
		wait.ctx, wait.cancel = context.WithTimeout(ctx, duration)
	}
	return wait
}

func (w *StatusWait) Context() context.Context { return w.ctx }

func (w *StatusWait) Close() { w.cancel() }

// ContextError distinguishes our deadline from cancellation by the caller.
// Call it only at transport/probe boundaries, never over ownership errors.
func (w *StatusWait) ContextError(id string) error {
	if errors.Is(w.ctx.Err(), context.DeadlineExceeded) && w.parent.Err() == nil {
		return w.timeout(id)
	}
	return w.parent.Err()
}

// Next is called only after a waiting adapter has ruled out readiness and
// terminal states. The adapter clock deadline takes precedence at this point.
func (w *StatusWait) Next(id string, interval time.Duration) error {
	if core.ClockNow(w.clock).After(w.deadline) {
		return w.timeout(id)
	}
	select {
	case <-w.ctx.Done():
		return w.ContextError(id)
	case <-time.After(interval):
		return nil
	}
}
