package shared

import (
	"context"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type DelegatedStatusRequest struct {
	Wait         bool
	WaitTimeout  time.Duration
	Now          func() time.Time
	Observe      func(context.Context) (core.StatusView, bool, error)
	TimeoutError func() error
}

// PollDelegatedStatus keeps complete views and terminal decisions with the
// provider. Observe's stop result ends polling without changing view.Ready.
func PollDelegatedStatus(ctx context.Context, req DelegatedStatusRequest) (core.StatusView, error) {
	deadline := req.Now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = req.Now().Add(5 * time.Minute)
	}
	for {
		view, stop, err := req.Observe(ctx)
		if err != nil {
			return core.StatusView{}, err
		}
		if !req.Wait || view.Ready || stop {
			return view, nil
		}
		if req.Now().After(deadline) {
			return core.StatusView{}, req.TimeoutError()
		}
		select {
		case <-ctx.Done():
			return core.StatusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
