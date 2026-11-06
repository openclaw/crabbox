package cli

import "time"

// The foreground timeline measures exclusive wall time, including the waits
// for background work. It never adds concurrent request durations together.
type runTimeline struct {
	started time.Time
	last    time.Time
	name    string
	phases  []RunnerPhase
}

func newRunTimeline(now time.Time) *runTimeline {
	return &runTimeline{started: now, last: now, name: "local.prepare"}
}

func (t *runTimeline) advance(name string) {
	t.advanceAt(name, time.Now())
}

func (t *runTimeline) advanceAt(name string, now time.Time) {
	// Round cumulative boundaries, so even sub-millisecond phases account for
	// exactly the elapsed total rather than accumulating truncation losses.
	ms := durationMillisecondsCeil(now.Sub(t.started)) - durationMillisecondsCeil(t.last.Sub(t.started))
	if ms > 0 {
		t.phases = append(t.phases, RunnerPhase{Name: t.name, Ms: ms})
	}
	t.last, t.name = now, name
}

func (t *runTimeline) section(name string) func() {
	previous := t.name
	t.advance(name)
	return func() { t.advance(previous) }
}

func (t *runTimeline) snapshot(now time.Time) (int64, []RunnerPhase) {
	copy := *t
	copy.phases = append([]RunnerPhase(nil), t.phases...)
	copy.advanceAt(t.name, now)
	return durationMillisecondsCeil(now.Sub(t.started)), copy.phases
}
