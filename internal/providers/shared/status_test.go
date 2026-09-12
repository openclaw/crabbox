package shared

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestPollDelegatedStatusObservationPrecedence(t *testing.T) {
	failure := errors.New("observation failed")
	for _, tc := range []struct {
		name              string
		wait, ready, stop bool
		err               error
	}{
		{name: "single observation"},
		{name: "ready", wait: true, ready: true},
		{name: "provider stop", wait: true, stop: true},
		{name: "error before ready and stop", wait: true, ready: true, stop: true, err: failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			view := core.StatusView{ID: "lease", Slug: "raw slug", Provider: "fixture", State: "stopped", Ready: tc.ready, WorkRoot: "/provider/work", ProviderResourceID: "immutable", Host: "host.example", SSHPort: "0022", SSHFallbackPorts: []string{"2200"}, Labels: map[string]string{"slug": "provider slug"}}
			calls, clockCalls := 0, 0
			got, err := PollDelegatedStatus(ctx, DelegatedStatusRequest{
				Wait: tc.wait, WaitTimeout: time.Second,
				Now: func() time.Time { clockCalls++; return time.Unix(0, 0) },
				Observe: func(observed context.Context) (core.StatusView, bool, error) {
					calls++
					if observed != ctx {
						t.Fatal("observation context changed")
					}
					return view, tc.stop, tc.err
				},
				TimeoutError: func() error { t.Fatal("terminal observation checked timeout"); return nil },
			})
			if err != tc.err || calls != 1 || clockCalls != 1 {
				t.Fatalf("err=%v calls=%d clock=%d", err, calls, clockCalls)
			}
			if tc.err != nil {
				if !reflect.DeepEqual(got, core.StatusView{}) {
					t.Fatalf("error returned partial view: %#v", got)
				}
				return
			}
			if !reflect.DeepEqual(got, view) {
				t.Fatalf("view changed: %#v want %#v", got, view)
			}
			got.Labels["slug"] = "same map"
			if view.Labels["slug"] != "same map" {
				t.Fatal("provider labels were rebuilt")
			}
		})
	}
}

func TestPollDelegatedStatusDeadlineOrdering(t *testing.T) {
	timeoutErr := errors.New("provider timeout")
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		times   []time.Duration
		want    error
	}{
		{name: "positive equality waits", timeout: time.Second, times: []time.Duration{0, time.Second}, want: context.Canceled},
		{name: "strictly after wins cancellation", timeout: time.Second, times: []time.Duration{0, time.Second + 1}, want: timeoutErr},
		{name: "zero uses second clock and five minutes", times: []time.Duration{0, 10 * time.Second, 310 * time.Second}, want: context.Canceled},
		{name: "negative uses second clock and five minutes", timeout: -time.Second, times: []time.Duration{0, 10 * time.Second, 310*time.Second + 1}, want: timeoutErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			clockCalls, observations, timeouts := 0, 0, 0
			got, err := PollDelegatedStatus(ctx, DelegatedStatusRequest{Wait: true, WaitTimeout: tc.timeout,
				Now: func() time.Time {
					if clockCalls >= len(tc.times) {
						t.Fatal("extra clock read")
					}
					v := time.Unix(0, 0).Add(tc.times[clockCalls])
					clockCalls++
					return v
				},
				Observe: func(context.Context) (core.StatusView, bool, error) {
					observations++
					cancel()
					return core.StatusView{ID: "not-ready"}, false, nil
				},
				TimeoutError: func() error { timeouts++; return timeoutErr },
			})
			wantTimeouts := 0
			if tc.want == timeoutErr {
				wantTimeouts = 1
			}
			if err != tc.want || observations != 1 || clockCalls != len(tc.times) || timeouts != wantTimeouts || !reflect.DeepEqual(got, core.StatusView{}) {
				t.Fatalf("err=%v observations=%d clock=%d timeouts=%d view=%#v", err, observations, clockCalls, timeouts, got)
			}
		})
	}
}

func TestPollDelegatedStatusReadyAfterPollInterval(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var observed []time.Time
	got, err := PollDelegatedStatus(ctx, DelegatedStatusRequest{Wait: true, WaitTimeout: time.Minute, Now: time.Now,
		Observe: func(context.Context) (core.StatusView, bool, error) {
			observed = append(observed, time.Now())
			return core.StatusView{ID: "latest", Ready: len(observed) == 2}, false, nil
		},
		TimeoutError: func() error { return errors.New("unexpected timeout") },
	})
	if err != nil || !got.Ready || got.ID != "latest" || len(observed) != 2 {
		t.Fatalf("view=%#v err=%v observations=%d", got, err, len(observed))
	}
	if observed[1].Sub(observed[0]) < 2*time.Second {
		t.Fatal("poll interval shortened")
	}
}
