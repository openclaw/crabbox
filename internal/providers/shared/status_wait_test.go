package shared

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestStatusWaitContextBoundaries(t *testing.T) {
	timeout := func(id string) error { return fmt.Errorf("waiting for %s", id) }
	t.Run("own deadline uses current identifier", func(t *testing.T) {
		wait := NewStatusWait(context.Background(), core.StatusRequest{Wait: true, WaitTimeout: time.Millisecond}, nil, timeout)
		defer wait.Close()
		<-wait.Context().Done()
		for _, id := range []string{"requested-slug", "resolved-id"} {
			if err := wait.ContextError(id); err == nil || err.Error() != "waiting for "+id {
				t.Fatalf("ContextError(%q) = %v", id, err)
			}
		}
	})
	t.Run("parent cancellation wins over expired child", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		wait := NewStatusWait(parent, core.StatusRequest{Wait: true, WaitTimeout: time.Millisecond}, nil, timeout)
		defer wait.Close()
		<-wait.Context().Done()
		cancel()
		if err := wait.ContextError("id"); err != context.Canceled {
			t.Fatalf("ContextError = %v", err)
		}
	})
	t.Run("parent deadline stays a context error", func(t *testing.T) {
		parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		wait := NewStatusWait(parent, core.StatusRequest{Wait: true}, nil, timeout)
		defer wait.Close()
		if err := wait.ContextError("id"); err != context.DeadlineExceeded {
			t.Fatalf("ContextError = %v", err)
		}
	})
	t.Run("nonwaiting requests keep the parent context", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		wait := NewStatusWait(parent, core.StatusRequest{WaitTimeout: time.Nanosecond}, nil, timeout)
		if wait.Context() != parent || wait.ContextError("id") != nil {
			t.Fatal("nonwaiting request received a child context or an error")
		}
		wait.Close()
		if parent.Err() != nil {
			t.Fatal("closing the wait canceled its parent")
		}
		cancel()
		if err := wait.ContextError("id"); err != context.Canceled {
			t.Fatalf("ContextError = %v", err)
		}
	})
}

func TestStatusWaitNextUsesClockAndCancellation(t *testing.T) {
	expired := errors.New("adapter timeout")
	for _, duration := range []time.Duration{0, -time.Second, time.Minute} {
		t.Run(duration.String(), func(t *testing.T) {
			clock := &sandboxTestClock{current: time.Now()}
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			wait := NewStatusWait(parent, core.StatusRequest{Wait: true, WaitTimeout: duration}, clock, func(string) error { return expired })
			defer wait.Close()
			if err := wait.ContextError("id"); err != nil {
				t.Fatalf("healthy context = %v", err)
			}
			if duration <= 0 {
				duration = 5 * time.Minute
			}
			clock.current = clock.current.Add(duration)
			if err := wait.Next("id", 0); err != nil {
				t.Fatalf("exact deadline should still poll: %v", err)
			}
			cancel()
			if err := wait.Next("id", time.Hour); err != context.Canceled {
				t.Fatalf("canceled timer wait = %v", err)
			}
			clock.current = clock.current.Add(time.Nanosecond)
			if err := wait.Next("id", time.Hour); err != expired {
				t.Fatalf("elapsed adapter deadline should take precedence: %v", err)
			}
		})
	}
}

func TestContextStatusWaitNextClassifiesDeadlinesAndCancellation(t *testing.T) {
	expired := errors.New("wait timeout")
	for _, tc := range []struct {
		name                              string
		parentDeadline, cancelAfterExpiry bool
		want                              error
	}{
		{name: "own timeout", want: expired},
		{name: "parent deadline", parentDeadline: true, want: context.DeadlineExceeded},
		{name: "parent cancellation after own timeout", cancelAfterExpiry: true, want: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.parentDeadline {
				var stop context.CancelFunc
				parent, stop = context.WithDeadline(parent, time.Now().Add(-time.Second))
				defer stop()
			}
			wait := NewContextStatusWait(parent, core.StatusRequest{Wait: true, WaitTimeout: time.Millisecond}, func(string) error { return expired })
			defer wait.Close()
			<-wait.Context().Done()
			if tc.cancelAfterExpiry {
				cancel()
			}
			if err := wait.Next("sandbox", time.Hour); err != tc.want {
				t.Fatalf("Next=%v, want %v", err, tc.want)
			}
		})
	}
}

func TestStatusWaitPollPreservesObservedResults(t *testing.T) {
	ownershipErr := errors.New("ownership mismatch")
	for _, tc := range []struct {
		name       string
		wait, done bool
		ready      bool
		err        error
		wantErr    error
		wantView   bool
	}{
		{name: "nonwaiting snapshot", wantView: true},
		{name: "waiting cancellation", wait: true, wantErr: context.Canceled},
		{name: "observed readiness", wait: true, ready: true, wantView: true},
		{name: "terminal snapshot", wait: true, done: true, wantView: true},
		{name: "ownership error", wait: true, err: ownershipErr, wantErr: ownershipErr, wantView: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			cancel()
			clock := &sandboxTestClock{current: time.Unix(1000, 0)}
			wait := NewStatusWait(parent, core.StatusRequest{Wait: tc.wait}, clock, func(string) error {
				t.Fatal("unexpired adapter clock reached the timeout handler")
				return nil
			})
			defer wait.Close()
			observed := core.StatusView{ID: "lease", Host: "vm.example", Ready: tc.ready, Labels: map[string]string{"scope": "example"}}
			calls := 0
			view, err := wait.Poll("resolved-id", time.Hour, func(ctx context.Context) (core.StatusView, bool, error) {
				if ctx != wait.Context() {
					t.Fatal("observation did not receive the wait's context")
				}
				calls++
				return observed, tc.done, tc.err
			})
			wantView := core.StatusView{}
			if tc.wantView {
				wantView = observed
			}
			if calls != 1 || err != tc.wantErr || !reflect.DeepEqual(view, wantView) {
				t.Fatalf("calls=%d view=%+v err=%v, want view=%+v err=%v", calls, view, err, wantView, tc.wantErr)
			}
		})
	}
}

func TestStatusWaitPollUsesBoundedContextAndResolvedID(t *testing.T) {
	timeoutErr := errors.New("adapter timeout")
	clock := &sandboxTestClock{current: time.Unix(1000, 0)}
	wait := NewStatusWait(context.Background(), core.StatusRequest{Wait: true, WaitTimeout: time.Millisecond}, clock, func(id string) error {
		if id != "resolved-id" {
			t.Fatalf("timeout identifier=%q", id)
		}
		return timeoutErr
	})
	defer wait.Close()
	<-wait.Context().Done()
	view, err := wait.Poll("resolved-id", time.Hour, func(ctx context.Context) (core.StatusView, bool, error) {
		if ctx != wait.Context() || ctx.Err() != context.DeadlineExceeded {
			t.Fatal("observation did not receive the expired request budget")
		}
		return core.StatusView{ID: "lease", State: "starting"}, false, nil
	})
	if err != timeoutErr || !reflect.DeepEqual(view, core.StatusView{}) {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}
