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
	now      func() time.Time
	deadline time.Time
	wait     bool
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
		now:      func() time.Time { return core.ClockNow(clock) },
		deadline: core.ClockNow(clock).Add(duration),
		wait:     req.Wait,
		timeout:  timeout,
	}
	if req.Wait {
		wait.ctx, wait.cancel = context.WithTimeout(ctx, duration)
	}
	return wait
}

func (w *StatusWait) Context() context.Context { return w.ctx }

func (w *StatusWait) Close() { w.cancel() }

// Poll shares observation sequencing while the adapter retains transport-error
// classification and ownership checks. done permits a final non-ready result.
func (w *StatusWait) Poll(id string, interval time.Duration, observe func(context.Context) (core.StatusView, bool, error)) (core.StatusView, error) {
	for {
		view, done, err := observe(w.ctx)
		if done || err != nil {
			return view, err
		}
		if !w.wait || view.Ready {
			return view, nil
		}
		if err := w.Next(id, interval); err != nil {
			return core.StatusView{}, err
		}
	}
}

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
	if w.now().After(w.deadline) {
		return w.timeout(id)
	}
	if err := core.SleepContext(w.ctx, interval); err != nil {
		return w.ContextError(id)
	}
	return nil
}
