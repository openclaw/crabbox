package remoteruntime

import (
	"testing"
	"time"
)

func TestTermGraceOnlyWaitsForLiveWork(t *testing.T) {
	for _, tc := range []struct {
		name      string
		livePolls int
		want      time.Duration
	}{
		{"completed", 0, 0}, {"exits during grace", 2, 20 * time.Millisecond}, {"still live", 1000, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Unix(0, 0)
			clock := start
			polls := 0
			waitTermGrace(5*time.Second, func() bool { polls++; return polls <= tc.livePolls }, func() time.Time { return clock }, func(delay time.Duration) { clock = clock.Add(delay) })
			if got := clock.Sub(start); got != tc.want {
				t.Fatalf("waited %s; want %s", got, tc.want)
			}
		})
	}
}
