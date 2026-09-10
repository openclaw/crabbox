package cli

import (
	"testing"
	"time"
)

type clockNowFunc func() time.Time

func (fn clockNowFunc) Now() time.Time { return fn() }

type clockNowTypedNil struct{}

var clockNowTypedNilSentinel = time.Date(2026, time.September, 7, 12, 34, 56, 0, time.UTC)

func (*clockNowTypedNil) Now() time.Time { return clockNowTypedNilSentinel }

type clockNowBackend struct {
	rt Runtime
}

func TestClockNowNilUsesSystemTime(t *testing.T) {
	before := time.Now()
	got := ClockNow(nil)
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("ClockNow(nil) = %v, want between %v and %v", got, before, after)
	}
}

func TestClockNowCallsInjectedClockOnceAndAdvances(t *testing.T) {
	first := time.Date(2026, time.September, 7, 1, 2, 3, 0, time.UTC)
	calls := 0
	clock := clockNowFunc(func() time.Time {
		now := first.Add(time.Duration(calls) * time.Second)
		calls++
		return now
	})
	for i, want := range []time.Time{first, first.Add(time.Second)} {
		if got := ClockNow(clock); got != want {
			t.Fatalf("ClockNow() call %d = %v, want %v", i+1, got, want)
		}
		if calls != i+1 {
			t.Fatalf("clock calls after invocation %d = %d, want %d", i+1, calls, i+1)
		}
	}
}

func TestClockNowDispatchesTypedNilClock(t *testing.T) {
	var typedNil *clockNowTypedNil
	var clock Clock = typedNil
	if got := ClockNow(clock); got != clockNowTypedNilSentinel {
		t.Fatalf("ClockNow(typed nil) = %v, want %v", got, clockNowTypedNilSentinel)
	}
}

func TestClockNowPreservesExactValueAndLocation(t *testing.T) {
	location := time.FixedZone("clock-now-test", 5*60*60+30*60)
	want := time.Date(2026, time.September, 7, 8, 9, 10, 11, location)
	if got := ClockNow(clockNowFunc(func() time.Time { return want })); got != want {
		t.Fatalf("ClockNow() = %#v, want exact value %#v", got, want)
	}
}

func TestClockNowBackendCallbackObservesRuntimeClockReplacement(t *testing.T) {
	first := time.Date(2026, time.September, 7, 1, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	backend := &clockNowBackend{rt: Runtime{Clock: clockNowFunc(func() time.Time { return first })}}
	now := func() time.Time { return ClockNow(backend.rt.Clock) }
	if got := now(); got != first {
		t.Fatalf("first callback time = %v, want %v", got, first)
	}
	backend.rt.Clock = clockNowFunc(func() time.Time { return second })
	if got := now(); got != second {
		t.Fatalf("callback after clock replacement = %v, want %v", got, second)
	}
}

func TestRuntimeDoesNotImplementClock(t *testing.T) {
	if _, ok := any(Runtime{}).(Clock); ok {
		t.Fatal("Runtime unexpectedly implements Clock")
	}
	if _, ok := any(&Runtime{}).(Clock); ok {
		t.Fatal("*Runtime unexpectedly implements Clock")
	}
}
