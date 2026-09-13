package shared

import (
	"context"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// PollStatus shares observation-only status waiting without bounding provider
// requests. The adapter owns resolution, views, and terminal/error policy; done
// returns a final observation even when it is not ready. Observations precede
// deadline and cancellation checks, including the first observation.
func PollStatus(
	ctx context.Context,
	req core.StatusRequest,
	now func() time.Time,
	observe func(context.Context) (view core.StatusView, done bool, err error),
	timeout func() error,
) (core.StatusView, error) {
	deadline := now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = now().Add(5 * time.Minute)
	}
	wait := StatusWait{
		parent: ctx, ctx: ctx, cancel: func() {},
		now: now, deadline: deadline, wait: req.Wait,
		timeout: func(string) error { return timeout() },
	}
	return wait.Poll("", 2*time.Second, observe)
}
