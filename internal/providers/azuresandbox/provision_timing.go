package azuresandbox

import (
	"context"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Only observations from this synchronous warmup are collected. Durations use
// the client's monotonic clock; service internals are not inferred from them.
type sandboxProvisionTimingKey struct{}
type sandboxProvisionStep struct {
	name     string
	duration time.Duration
	count    int64
	at       time.Time
}
type sandboxProvisionTiming struct {
	now   func() time.Time
	steps []sandboxProvisionStep
}

func measureSandboxProvisionStep(ctx context.Context, name string) func() {
	timing, _ := ctx.Value(sandboxProvisionTimingKey{}).(*sandboxProvisionTiming)
	if timing == nil {
		return func() {}
	}
	// Keep service text, resource paths and command arguments out of telemetry.
	switch name {
	case "credential", "create", "inspect", "list", "resume",
		"poll_wait.creating", "poll_wait.starting", "poll_wait.resuming",
		"poll_wait.stopping", "poll_wait.suspending", "poll_wait.inactive":
	default:
		name = "other_request"
	}
	started := timing.now()
	return func() {
		ended := timing.now()
		duration := ended.Sub(started)
		if duration < 0 {
			return
		}
		for i := range timing.steps {
			if timing.steps[i].name == name {
				timing.steps[i].duration += duration
				timing.steps[i].count++
				timing.steps[i].at = ended
				return
			}
		}
		timing.steps = append(timing.steps, sandboxProvisionStep{name: name, duration: duration, count: 1, at: ended})
	}
}

func sandboxPollState(state string) string {
	switch state {
	case "Creating", "Starting", "Resuming", "Stopping", "Suspending":
		return strings.ToLower(state)
	default:
		return "inactive"
	}
}

func (t *sandboxProvisionTiming) phases() []core.RunnerPhase {
	phases := make([]core.RunnerPhase, 0, len(t.steps))
	for _, step := range t.steps {
		phase := core.RunnerPhase{Name: "provider.sandbox." + step.name, Ms: step.duration.Milliseconds()}
		if step.name == "create" || strings.HasPrefix(step.name, "poll_wait.") {
			phase.Opaque = true
			phase.Reason = "service does not expose separate disk attachment or boot timings"
		}
		phases = append(phases, phase)
	}
	return phases
}

func (t *sandboxProvisionTiming) events() []core.CreationEvent {
	events := make([]core.CreationEvent, 0, len(t.steps))
	for _, step := range t.steps {
		ms := float64(step.duration) / float64(time.Millisecond)
		events = append(events, core.CreationEvent{Phase: "provider_step", Step: "provider.sandbox." + step.name, Source: "client", At: step.at.UTC().Format(time.RFC3339Nano), DurationMs: &ms, Count: step.count})
	}
	return events
}
