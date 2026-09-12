package shared

import (
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
