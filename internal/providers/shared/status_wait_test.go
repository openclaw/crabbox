package shared

import (
	"context"
	"errors"
	"fmt"
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
