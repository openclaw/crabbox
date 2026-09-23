package shared

import (
	"math"
	"testing"
	"time"
)

func TestSecondsWithGrace(t *testing.T) {
	for _, grace := range []time.Duration{0, time.Second, 30 * time.Second, time.Duration(math.MaxInt64)} {
		t.Run(grace.String(), func(t *testing.T) {
			max := (math.MaxInt64 - int64(grace)) / int64(time.Second)
			for _, seconds := range []int64{0, max} {
				got, ok := SecondsWithGrace(seconds, grace)
				if !ok || got != time.Duration(seconds)*time.Second+grace {
					t.Fatalf("valid budget: seconds=%d grace=%s got=%s ok=%t", seconds, grace, got, ok)
				}
			}
			for _, seconds := range []int64{-1, max + 1, math.MaxInt64} {
				if got, ok := SecondsWithGrace(seconds, grace); ok || got != 0 {
					t.Fatalf("invalid budget accepted: seconds=%d grace=%s", seconds, grace)
				}
			}
		})
	}
	if _, ok := SecondsWithGrace(1, -time.Second); ok {
		t.Fatal("negative grace accepted")
	}
}
