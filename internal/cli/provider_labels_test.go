package cli

import (
	"regexp"
	"testing"
	"time"
)

func TestDirectLeaseLabelsAreProviderSafe(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Class:       "standard",
		Profile:     "default",
		ProviderKey: "crabbox-cbx-abcdef123456",
		ServerType:  "cpx62",
		Desktop:     true,
		Browser:     true,
		TTL:         15 * time.Minute,
		IdleTimeout: 4 * time.Minute,
	}
	labels := directLeaseLabels(cfg, "cbx_abcdef123456", "blue-lobster", "hetzner", "", true, now)
	safe := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	for key, value := range labels {
		if !safe.MatchString(value) {
			t.Fatalf("label %s=%q is not provider-safe", key, value)
		}
	}
	if labels["created_at"] != "1777636800" || labels["last_touched_at"] != "1777636800" {
		t.Fatalf("timestamps are not unix seconds: %#v", labels)
	}
	if labels["idle_timeout_secs"] != "240" || labels["idle_timeout"] != "240" {
		t.Fatalf("idle timeout labels = %#v, want seconds", labels)
	}
	if labels["ttl_secs"] != "900" {
		t.Fatalf("ttl_secs=%q want 900", labels["ttl_secs"])
	}
	if labels["desktop"] != "true" || labels["browser"] != "true" {
		t.Fatalf("capability labels missing: %#v", labels)
	}
	if labels["expires_at"] != "1777637040" {
		t.Fatalf("expires_at=%q want idle expiry", labels["expires_at"])
	}
}

func TestTouchDirectLeaseLabelsMovesExpiryForwardToTTLCap(t *testing.T) {
	created := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	touched := created.Add(3 * time.Minute)
	cfg := Config{TTL: 15 * time.Minute, IdleTimeout: 30 * time.Minute}
	labels := directLeaseLabels(Config{
		Class:       "standard",
		Profile:     "default",
		ProviderKey: "crabbox-cbx-abcdef123456",
		ServerType:  "cpx62",
		TTL:         15 * time.Minute,
		IdleTimeout: 4 * time.Minute,
	}, "cbx_abcdef123456", "blue-lobster", "hetzner", "", true, created)

	got := touchDirectLeaseLabels(labels, cfg, "running", touched)
	if got["state"] != "running" {
		t.Fatalf("state=%q want running", got["state"])
	}
	if got["last_touched_at"] != "1777636980" {
		t.Fatalf("last_touched_at=%q", got["last_touched_at"])
	}
	if got["idle_timeout_secs"] != "240" {
		t.Fatalf("idle_timeout_secs=%q should preserve existing lease timeout", got["idle_timeout_secs"])
	}
	if got["expires_at"] != "1777637220" {
		t.Fatalf("expires_at=%q want touched+idle", got["expires_at"])
	}

	got = touchDirectLeaseLabels(got, cfg, "ready", created.Add(14*time.Minute))
	if got["expires_at"] != "1777637700" {
		t.Fatalf("expires_at=%q want ttl cap", got["expires_at"])
	}
}

func TestParseLeaseLabelTimeAcceptsLegacyRFC3339(t *testing.T) {
	legacy := "2026-05-01T12:00:00Z"
	got, ok := parseLeaseLabelTime(legacy)
	if !ok || got.Format(time.RFC3339) != legacy {
		t.Fatalf("parseLeaseLabelTime(%q)=%s,%v", legacy, got, ok)
	}
}

func TestProviderLabelDisplayAndDurationHelpers(t *testing.T) {
	if got := leaseLabelTimeDisplay("1777636800"); got != "2026-05-01T12:00:00Z" {
		t.Fatalf("leaseLabelTimeDisplay=%q", got)
	}
	if got := leaseLabelTimeDisplay("not-a-time"); got != "" {
		t.Fatalf("invalid leaseLabelTimeDisplay=%q", got)
	}
	for _, tc := range []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"240", 4 * time.Minute, true},
		{"4m", 4 * time.Minute, true},
		{"0", 0, false},
		{"bad", 0, false},
		{"", 0, false},
	} {
		got, ok := parseDurationSecondsLabel(tc.value)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("parseDurationSecondsLabel(%q)=%s,%v want %s,%v", tc.value, got, ok, tc.want, tc.ok)
		}
	}
	if got := durationSecondsLabel(0); got != "" {
		t.Fatalf("zero duration label=%q", got)
	}
	if got := leaseLabelDurationDisplay("240", "5m"); got != "4m0s" {
		t.Fatalf("duration display primary=%q", got)
	}
	if got := leaseLabelDurationDisplay("bad", "5m"); got != "5m0s" {
		t.Fatalf("duration display fallback=%q", got)
	}
	if got := leaseLabelDurationDisplay("bad", "also-bad"); got != "" {
		t.Fatalf("invalid duration display=%q", got)
	}
}

func TestTouchDirectLeaseLabelsFallsBackForMalformedStoredValues(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	cfg := Config{TTL: 10 * time.Minute, IdleTimeout: 2 * time.Minute}
	got := touchDirectLeaseLabels(map[string]string{
		"created_at":        "bad",
		"idle_timeout_secs": "bad",
		"ttl_secs":          "bad",
		"slug":              "blue lobster",
	}, cfg, "", now)
	if got["created_at"] != "1777636800" || got["last_touched_at"] != "1777636800" {
		t.Fatalf("timestamps did not fall back to now: %#v", got)
	}
	if got["idle_timeout_secs"] != "120" || got["ttl_secs"] != "600" || got["expires_at"] != "1777636920" {
		t.Fatalf("durations did not fall back to cfg: %#v", got)
	}
	if got["slug"] != "blue_lobster" {
		t.Fatalf("slug was not sanitized: %#v", got)
	}
}

func TestSanitizeProviderLabelValueEdgeCases(t *testing.T) {
	if got := sanitizeProviderLabelValue("..."); got != "unknown" {
		t.Fatalf("punctuation-only value=%q", got)
	}
	long := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ01234567890123456789"
	if got := sanitizeProviderLabelValue(long); len(got) != 63 {
		t.Fatalf("long value length=%d value=%q", len(got), got)
	}
}
