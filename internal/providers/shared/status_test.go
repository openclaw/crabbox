package shared

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestPollStatusObservationPrecedesDeadlineAndCancellation(t *testing.T) {
	providerErr := errors.New("provider observation failed")
	for _, tc := range []struct {
		name       string
		wait, done bool
		view       core.StatusView
		err        error
	}{
		{name: "single observation", view: core.StatusView{ID: "lease", State: "starting"}},
		{name: "ready observation", wait: true, view: core.StatusView{ID: "lease", Ready: true}},
		{name: "terminal observation", wait: true, done: true, view: core.StatusView{ID: "lease", State: "stopped"}},
		{name: "error with view", wait: true, view: core.StatusView{ID: "lease", Ready: true}, err: providerErr},
		{name: "complete provider view", view: core.StatusView{ID: "lease", Slug: "raw slug", Provider: "fixture", State: "stopped", WorkRoot: "/provider/work", ProviderResourceID: "immutable", Host: "host.example", SSHPort: "0022", SSHFallbackPorts: []string{"2200"}, Labels: map[string]string{"slug": "provider slug"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			now := time.Unix(1000, 0)
			observations := 0
			got, err := PollStatus(ctx, core.StatusRequest{Wait: tc.wait, WaitTimeout: time.Second}, func() time.Time { return now }, func(gotCtx context.Context) (core.StatusView, bool, error) {
				if gotCtx != ctx {
					t.Fatal("provider observation received a different context")
				}
				observations++
				now = now.Add(time.Minute)
				return tc.view, tc.done, tc.err
			}, func() error {
				t.Fatal("completed observation reached the timeout handler")
				return nil
			})
			if observations != 1 || !reflect.DeepEqual(got, tc.view) || err != tc.err {
				t.Fatalf("observations=%d view=%+v err=%v", observations, got, err)
			}
		})
	}
}

func TestPollStatusDeadlineAndCancellationPrecedence(t *testing.T) {
	timeoutErr := errors.New("status polling timed out")
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		elapsed time.Duration
		wantErr error
	}{
		{"before deadline", time.Minute, time.Second, context.Canceled},
		{"exact deadline", time.Minute, time.Minute, context.Canceled},
		{"past deadline", time.Minute, time.Minute + time.Nanosecond, timeoutErr},
		{"default before deadline", 0, 5*time.Minute - time.Second, context.Canceled},
		{"default past deadline", 0, 5*time.Minute + time.Second, timeoutErr},
		{"negative timeout uses default", -time.Minute, 5*time.Minute + time.Second, timeoutErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			now := time.Unix(1000, 0)
			view, err := PollStatus(ctx, core.StatusRequest{Wait: true, WaitTimeout: tc.timeout}, func() time.Time { return now }, func(gotCtx context.Context) (core.StatusView, bool, error) {
				if _, bounded := gotCtx.Deadline(); bounded {
					t.Fatal("polling imposed a deadline on the provider request")
				}
				now = now.Add(tc.elapsed)
				cancel(errors.New("caller-specific cancellation cause"))
				return core.StatusView{ID: "lease", State: "starting"}, false, nil
			}, func() error { return timeoutErr })
			if !reflect.DeepEqual(view, core.StatusView{}) || err != tc.wantErr {
				t.Fatalf("view=%+v err=%v, want empty view and %v", view, err, tc.wantErr)
			}
		})
	}
}

func TestPollStatusWaitsBetweenObservations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var first time.Time
	observations := 0
	view, err := PollStatus(ctx, core.StatusRequest{Wait: true}, time.Now, func(gotCtx context.Context) (core.StatusView, bool, error) {
		if gotCtx != ctx {
			t.Fatal("caller deadline context was replaced")
		}
		observations++
		if observations == 1 {
			first = time.Now()
			return core.StatusView{State: "starting"}, false, nil
		}
		if time.Since(first) < 1500*time.Millisecond {
			t.Fatal("status observations were not separated by the polling delay")
		}
		return core.StatusView{ID: "lease", Ready: true}, false, nil
	}, func() error { return errors.New("unexpected polling timeout") })
	if err != nil || !view.Ready || view.ID != "lease" || observations != 2 {
		t.Fatalf("view=%+v err=%v observations=%d", view, err, observations)
	}
}
