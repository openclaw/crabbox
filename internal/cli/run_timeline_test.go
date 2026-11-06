package cli

import (
	"testing"
	"time"
)

func TestRunTimelineAccountsForEveryMillisecond(t *testing.T) {
	start := time.Unix(100, 0)
	timeline := newRunTimeline(start)
	timeline.advanceAt("history.admit", start.Add(400*time.Microsecond))
	timeline.advanceAt("command", start.Add(1400*time.Microsecond))
	timeline.advanceAt("history.finish", start.Add(3*time.Millisecond))
	total, phases := timeline.snapshot(start.Add(4700 * time.Microsecond))
	if total != 5 {
		t.Fatalf("total=%d, want 5", total)
	}
	var sum int64
	for _, phase := range phases {
		if phase.Name == "unattributed" || phase.Ms <= 0 {
			t.Fatalf("invalid phase: %+v", phase)
		}
		sum += phase.Ms
	}
	if sum != total || phases[len(phases)-1].Name != "history.finish" {
		t.Fatalf("total=%d sum=%d phases=%+v", total, sum, phases)
	}
	if len(timeline.phases) != len(phases)-1 {
		t.Fatal("snapshot modified live timeline")
	}
}
