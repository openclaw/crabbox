package serve

import (
	"bytes"
	"context"
	"errors"
	"math"
	"runtime"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/localcontainer"
)

type availabilityStatsFunc func(context.Context, []localcontainer.ServedContainer) (float64, error)

func (f availabilityStatsFunc) CPUCores(ctx context.Context, items []localcontainer.ServedContainer) (float64, error) {
	return f(ctx, items)
}

func fixedHostSignals(cpu float64) hostSignals {
	return hostSignals{
		cpu:    func(context.Context) *float64 { return floatPtr(cpu) },
		memory: func() string { return "normal" },
		idle:   func(context.Context) *float64 { return floatPtr(3600) },
		power:  func(context.Context) string { return "ac" },
	}
}

func TestAvailabilityContainerFailureUsesRawCPU(t *testing.T) {
	owned := []localcontainer.ServedContainer{{ID: "owned", Running: true, Labels: map[string]string{"crabbox.serve": "true", "crabbox.serve.machine": "machine"}}}
	for _, tc := range []struct {
		name      string
		inventory containerInventory
		stats     containerCPUStats
	}{
		{"missing inventory", knownInventory(nil), nil},
		{"failed inventory", func(context.Context) ([]localcontainer.ServedContainer, error) { return nil, errors.New("unavailable") }, nil},
		{"missing stats", knownInventory(owned), nil},
		{"failed stats", knownInventory(owned), availabilityStatsFunc(func(context.Context, []localcontainer.ServedContainer) (float64, error) {
			return 0, errors.New("unavailable")
		})},
		{"invalid stats", knownInventory(owned), availabilityStatsFunc(func(context.Context, []localcontainer.ServedContainer) (float64, error) { return math.NaN(), nil })},
		{"slow stats", knownInventory(owned), availabilityStatsFunc(func(ctx context.Context, _ []localcontainer.ServedContainer) (float64, error) {
			<-ctx.Done()
			return 0, ctx.Err()
		})},
	} {
		for _, cpu := range []float64{.2, .99} {
			t.Run(tc.name+"/"+map[bool]string{true: "busy", false: "available"}[cpu > .5], func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()
				s := (&hostSampler{}).sampleSignals(ctx, tc.inventory, tc.stats, "machine", fixedHostSignals(cpu))
				if s.cpu == nil || *s.cpu != cpu || !s.containerCPUUnknown || s.idle == nil {
					t.Fatalf("lost raw host measurements: %+v", s)
				}
				var window availabilityWindow
				var result availability
				for seconds := 0; seconds <= 480; seconds += 10 {
					s.at = time.Unix(int64(seconds), 0)
					result = window.evaluate(defaultAvailabilityPolicy(false), s)
				}
				want := "available"
				if cpu > .5 {
					want = "busy"
				}
				if result.State != want || result.HostCPU == nil || math.Abs(*result.HostCPU-cpu) > 1e-9 {
					t.Fatalf("fallback changed policy decision: %+v", result)
				}
				if !strings.Contains(strings.Join(result.Reasons, ","), "serve container CPU unknown") {
					t.Fatalf("missing informational fallback reason: %+v", result)
				}
			})
		}
	}
}

func TestAvailabilityIdleFailurePreservesHostCPU(t *testing.T) {
	signals := fixedHostSignals(.9)
	signals.idle = func(context.Context) *float64 { return nil }
	s := (&hostSampler{}).sampleSignals(t.Context(), knownInventory([]localcontainer.ServedContainer{}), nil, "machine", signals)
	var window availabilityWindow
	var result availability
	for seconds := 0; seconds <= 180; seconds += 10 {
		s.at = time.Unix(int64(seconds), 0)
		result = window.evaluate(defaultAvailabilityPolicy(true), s)
	}
	if result.HostCPU == nil || math.Abs(*result.HostCPU-.9) > 1e-9 || result.UserIdleSeconds != nil || result.State == "available" {
		t.Fatalf("idle failure affected CPU or allowed placement: %+v", result)
	}
	if !strings.Contains(strings.Join(result.Reasons, ","), "user idle unknown") {
		t.Fatal(result)
	}
}

func TestAvailabilitySignalsStartIndependently(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	idleDone := make(chan struct{})
	signals := fixedHostSignals(.99)
	signals.cpu = func(ctx context.Context) *float64 {
		close(started)
		select {
		case <-idleDone:
			return floatPtr(.99)
		case <-ctx.Done():
			return nil
		}
	}
	signals.idle = func(ctx context.Context) *float64 {
		select {
		case <-started:
			close(idleDone)
			return floatPtr(3600)
		case <-ctx.Done():
			return nil
		}
	}
	s := (&hostSampler{}).sampleSignals(ctx, knownInventory([]localcontainer.ServedContainer{}), nil, "machine", signals)
	if s.cpu == nil || s.idle == nil || ctx.Err() != nil {
		t.Fatalf("slow CPU blocked idle measurement: %+v (%v)", s, ctx.Err())
	}
}

func TestAvailabilitySlowHostReadHasItsOwnBudget(t *testing.T) {
	signals := fixedHostSignals(.99)
	signals.cpu = func(ctx context.Context) *float64 {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 40*time.Second {
			t.Error("host CPU did not receive a full sampling budget")
		}
		timer := time.NewTimer(9 * time.Second) // Longer than the old shared deadline.
		defer timer.Stop()
		select {
		case <-timer.C:
			return floatPtr(.99)
		case <-ctx.Done():
			return nil
		}
	}
	s := (&hostSampler{}).sampleSignals(t.Context(), knownInventory([]localcontainer.ServedContainer{}), nil, "machine", signals)
	if s.cpu == nil || s.idle == nil || s.containerCPUUnknown {
		t.Fatalf("lost values during slow host read: %+v", s)
	}
}

func TestAvailabilitySubtractsOnlyOwnedRunningContainers(t *testing.T) {
	owned := map[string]string{"crabbox.serve": "true", "crabbox.serve.machine": "machine"}
	items := []localcontainer.ServedContainer{
		{ID: "owned", Running: true, Labels: owned},
		{ID: "stopped", Labels: owned},
		{ID: "unrelated", Running: true},
	}
	stats := availabilityStatsFunc(func(_ context.Context, selected []localcontainer.ServedContainer) (float64, error) {
		if len(selected) != 1 || selected[0].ID != "owned" {
			t.Errorf("unexpected CPU selection: %+v", selected)
		}
		return float64(runtime.NumCPU()) * .3, nil
	})
	s := (&hostSampler{}).sampleSignals(t.Context(), knownInventory(items), stats, "machine", fixedHostSignals(.9))
	if s.cpu == nil || math.Abs(*s.cpu-.6) > 1e-9 || s.containerCPUUnknown {
		t.Fatalf("owned CPU subtraction failed: %+v", s)
	}
}

func TestAvailabilitySlowSamplingWindow(t *testing.T) {
	// A 35-second read leaves headroom above top's observed cost on a loaded host.
	// Advance the fake clock rather than waiting several real minutes.
	cost := 35 * time.Second
	if availabilitySignalTimeout <= cost || availabilitySignalTimeout >= availabilitySampleMaxAge {
		t.Fatal("signal budget must cover loaded hosts within the freshness window")
	}
	for _, cpu := range []float64{.2, .99} {
		var window availabilityWindow
		var result availability
		start := time.Unix(0, 0)
		for elapsed := time.Duration(0); elapsed < 8*time.Minute; {
			began := start.Add(elapsed)
			elapsed += cost
			sample := idleSample(start.Add(elapsed))
			sample.cpu = floatPtr(cpu)
			result = window.evaluate(defaultAvailabilityPolicy(false), sample)
			if elapsed > 2*time.Minute && result.HostCPU == nil {
				t.Fatalf("slow sampler never fills window: %+v", result)
			}
			elapsed += availabilitySampleDelay(began, start.Add(elapsed))
		}
		want := "available"
		if cpu > .5 {
			want = "busy"
		}
		if result.State != want {
			t.Fatalf("slow sample state: %+v, want %s", result, want)
		}
	}
}

func TestAvailabilityTopSaturatedHost(t *testing.T) {
	first := "CPU usage: 99.0% user, 1.0% sys, 0.0% idle\n"
	for _, tc := range []struct {
		output string
		want   float64
	}{
		{"CPU usage: 29.96% user, 70.30% sys, 0.0% idle", 1},
		{"CPU usage: 35.99% user, 64.02% sys, 0.0% idle", 1},
		{"CPU usage: 35.99% user, 64.01% sys, 0.0% idle", 1},
		{"CPU usage: 10.0% user, 5.0% sys, 85.0% idle", .15},
	} {
		got := parseHostCPU(first + tc.output)
		if got == nil || math.Abs(*got-tc.want) > 1e-9 {
			t.Fatalf("%s: %v", tc.output, got)
		}
	}
	if parseHostCPU(first+"CPU usage: 190.0% user, 90.0% sys, 0.0% idle") != nil {
		t.Fatal("accepted invalid percentages")
	}
}

type availabilityRunnerFunc func(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error)

func (f availabilityRunnerFunc) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	return f(ctx, req)
}

func TestAvailabilityDebugOmitsPrivateData(t *testing.T) {
	t.Setenv("CRABBOX_SERVE_AVAILABILITY_DEBUG", "1")
	var output bytes.Buffer
	runner := availabilityCommandRunner{out: &output, CommandRunner: availabilityRunnerFunc(func(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error) {
		return core.LocalCommandResult{Stdout: "private-output", Stderr: "private-stderr"}, errors.New("private-error")
	})}
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	_, _ = runner.Run(ctx, core.LocalCommandRequest{Name: "private-executable", Args: []string{"private-argument"}})
	if strings.Contains(output.String(), "private") || !strings.Contains(output.String(), "signal=runtime outcome=timeout elapsed=") {
		t.Fatalf("unsafe or missing diagnostics: %s", &output)
	}
}
