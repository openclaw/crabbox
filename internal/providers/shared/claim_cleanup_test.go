package shared

import (
	"math"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestClaimIdleCleanupDue(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name     string
		lastUsed string
		idle     int
		due      bool
		reason   string
	}{
		{"disabled", "invalid", 0, false, "idle timeout disabled"},
		{"negative", "invalid", -1, false, "idle timeout disabled"},
		{"missing timestamp", "", 60, false, "invalid last-used time"},
		{"invalid timestamp", "invalid", 60, false, "invalid last-used time"},
		{"future activity", "2026-09-12T12:01:00Z", 60, false, "idle timeout not reached"},
		{"before deadline", "2026-09-12T11:59:00.000000001Z", 60, false, "idle timeout not reached"},
		{"at deadline", "2026-09-12T11:59:00Z", 60, true, "idle timeout"},
		{"past deadline", "2026-09-12T11:58:59Z", 60, true, "idle timeout"},
		{"offset timestamp", "2026-09-12T13:59:00+02:00", 60, true, "idle timeout"},
		{"trimmed timestamp", " \t2026-09-12T11:59:00Z\n", 60, true, "idle timeout"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			claim := core.LeaseClaim{IdleTimeoutSeconds: tt.idle, LastUsedAt: tt.lastUsed}
			due, reason := ClaimIdleCleanupDue(claim, now)
			if due != tt.due || reason != tt.reason {
				t.Fatalf("got (%v, %q), want (%v, %q)", due, reason, tt.due, tt.reason)
			}
		})
	}
}

func TestClaimIdleCleanupDueRejectsOverflow(t *testing.T) {
	for _, raw := range []int64{9223372037, 18446744074} {
		seconds := int(raw)
		if int64(seconds) != raw {
			continue // These persisted values cannot exist on a 32-bit host.
		}
		claim := core.LeaseClaim{IdleTimeoutSeconds: seconds, LastUsedAt: "2026-09-01T00:00:00Z"}
		due, reason := ClaimIdleCleanupDue(claim, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
		if due || reason != "invalid idle timeout" {
			t.Fatalf("seconds=%d: got (%v, %q), want (false, invalid idle timeout)", raw, due, reason)
		}
	}
}

func TestClaimIdleCleanupDueMaximumDuration(t *testing.T) {
	raw := int64(math.MaxInt64) / int64(time.Second)
	seconds := int(raw)
	if int64(seconds) != raw {
		t.Skip("maximum seconds do not fit int")
	}
	last := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	deadline := last.Add(time.Duration(raw) * time.Second)
	claim := core.LeaseClaim{LastUsedAt: " " + last.Format(time.RFC3339) + "\n", IdleTimeoutSeconds: seconds}
	for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		t.Run(offset.String(), func(t *testing.T) {
			due, reason := ClaimIdleCleanupDue(claim, deadline.Add(offset))
			wantReason := "idle timeout not reached"
			if offset >= 0 {
				wantReason = "idle timeout"
			}
			if due != (offset >= 0) || reason != wantReason {
				t.Fatalf("got (%v, %q), want (%v, %q)", due, reason, offset >= 0, wantReason)
			}
		})
	}
	claim.LastUsedAt = "invalid"
	if due, reason := ClaimIdleCleanupDue(claim, deadline); due || reason != "invalid last-used time" {
		t.Fatalf("invalid timestamp: (%v, %q)", due, reason)
	}
}
