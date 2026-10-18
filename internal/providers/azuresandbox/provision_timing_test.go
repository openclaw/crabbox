package azuresandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProvisionTimingUsesNonOverlappingClientDurations(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	timing := &sandboxProvisionTiming{now: func() time.Time { return now }}
	ctx := context.WithValue(t.Context(), sandboxProvisionTimingKey{}, timing)
	for _, step := range []struct {
		name    string
		elapsed time.Duration
	}{{"credential", 3 * time.Millisecond}, {"create", 7 * time.Millisecond}, {"inspect", 2 * time.Millisecond}, {"poll_wait.creating", time.Second}, {"inspect", 4 * time.Millisecond}} {
		finish := measureSandboxProvisionStep(ctx, step.name)
		now = now.Add(step.elapsed)
		finish()
	}
	var sum int64
	for _, p := range timing.phases() {
		sum += p.Ms
	}
	if sum != 1016 {
		t.Fatalf("partition = %d", sum)
	}
	events := timing.events()
	if len(events) != 4 || events[2].Count != 2 || *events[2].DurationMs != 6 {
		t.Fatalf("events=%+v", events)
	}
	if !timing.phases()[1].Opaque || !timing.phases()[3].Opaque {
		t.Fatal("unobservable service phases presented as exact")
	}
}

func TestWarmupProvisionTimingSuccessAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "uncertain-create"}[fail], func(t *testing.T) {
			f := &fixture{loseCreateResponse: fail}
			b, req := fixtureBackend(t, f)
			var stderr bytes.Buffer
			b.rt.Stderr = &stderr
			req.TimingJSON = true
			err := b.WarmupFixed(t.Context(), req)
			if (err != nil) != fail {
				t.Fatalf("err=%v", err)
			}
			var report core.TimingReport
			if err := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &report); err != nil {
				t.Fatal(err)
			}
			if report.Provider != providerName || (!fail && report.LeaseID != req.RequestedLeaseID) || (fail && report.LeaseID != "") || (report.ExitCode != 0) != fail {
				t.Fatalf("outcome=%+v", report)
			}
			counts := map[string]int64{}
			for _, e := range report.CreationEvents {
				counts[e.Step] = e.Count
			}
			if counts["provider.sandbox.credential"] == 0 || counts["provider.sandbox.create"] != 1 || counts["provider.sandbox.list"] == 0 {
				t.Fatalf("counts=%v", counts)
			}
			if strings.Contains(stderr.String(), "synthetic-token") || strings.Contains(stderr.String(), "unique-resource") {
				t.Fatal("private transport data leaked")
			}
			if f.creates != 1 {
				t.Fatalf("timing changed mutation count: %d", f.creates)
			}
		})
	}
}
