package cli

import (
	"strings"
	"testing"
	"time"
)

// A lease whose TTL ends before GitHub-runner hydration can finish must be
// called out before a runner is registered. The idle deadline in expires_at is
// renewed by heartbeats and must not trigger the warning.
func TestActionsLeaseLifetimeWarning(t *testing.T) {
	now := time.Date(2026, 11, 2, 19, 9, 36, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		created time.Time
		ttl     string
		expires time.Time
		wait    time.Duration
		want    string
	}{
		{"default TTL ends before the wait budget", now.Add(-5*time.Minute - 32*time.Second), "1800", now.Add(24*time.Minute + 28*time.Second), 40 * time.Minute, "reaches its TTL in 24m28s, before the --wait-timeout of 40m0s"},
		{"TTL already passed", now.Add(-time.Hour), "1800", now.Add(-time.Minute), 20 * time.Minute, "reaches its TTL in 0s"},
		{"long TTL with a near idle deadline", now.Add(-10 * time.Minute), "7200", now.Add(20 * time.Minute), 40 * time.Minute, ""},
		{"TTL ends exactly at the wait budget", now, "2400", now.Add(40 * time.Minute), 40 * time.Minute, ""},
		{"no recorded TTL", now, "", now.Add(time.Minute), 40 * time.Minute, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := Server{Labels: map[string]string{
				"created_at": LeaseLabelTime(tc.created),
				"expires_at": LeaseLabelTime(tc.expires),
			}}
			if tc.ttl != "" {
				server.Labels["ttl_secs"] = tc.ttl
			}
			got := actionsLeaseLifetimeWarning("cbx_123", server, now, tc.wait)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("unexpected warning: %s", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) || !strings.Contains(got, "cbx_123") || !strings.Contains(got, "--ttl") {
				t.Fatalf("warning %q, want it to contain %q, the lease, and the --ttl remedy", got, tc.want)
			}
		})
	}
}
