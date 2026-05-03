package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

func directLeaseLabels(cfg Config, leaseID, slug, provider, market string, keep bool, now time.Time) map[string]string {
	expiresAt := directLeaseExpiresAt(now, cfg)
	labels := map[string]string{
		"class":             cfg.Class,
		"crabbox":           "true",
		"created_by":        "crabbox",
		"keep":              fmt.Sprint(keep),
		"lease":             leaseID,
		"slug":              normalizeLeaseSlug(slug),
		"profile":           cfg.Profile,
		"provider_key":      cfg.ProviderKey,
		"provider":          provider,
		"target":            cfg.TargetOS,
		"server_type":       cfg.ServerType,
		"state":             "leased",
		"created_at":        leaseLabelTime(now),
		"last_touched_at":   leaseLabelTime(now),
		"idle_timeout":      durationSecondsLabel(cfg.IdleTimeout),
		"idle_timeout_secs": durationSecondsLabel(cfg.IdleTimeout),
		"ttl_secs":          durationSecondsLabel(cfg.TTL),
		"expires_at":        leaseLabelTime(expiresAt),
	}
	if market != "" {
		labels["market"] = market
	}
	if cfg.TargetOS == targetWindows {
		labels["windows_mode"] = cfg.WindowsMode
	}
	if cfg.Desktop {
		labels["desktop"] = "true"
	}
	if cfg.Browser {
		labels["browser"] = "true"
	}
	return sanitizeProviderLabels(labels)
}

func touchDirectLeaseLabels(labels map[string]string, cfg Config, state string, now time.Time) map[string]string {
	next := make(map[string]string, len(labels)+4)
	for key, value := range labels {
		next[key] = value
	}
	if state != "" {
		next["state"] = state
	}
	createdAt, ok := parseLeaseLabelTime(next["created_at"])
	if !ok {
		createdAt = now
		next["created_at"] = leaseLabelTime(createdAt)
	}
	idleTimeout := cfg.IdleTimeout
	if stored, ok := parseDurationSecondsLabel(next["idle_timeout_secs"]); ok {
		idleTimeout = stored
	} else if stored, ok := parseDurationSecondsLabel(next["idle_timeout"]); ok {
		idleTimeout = stored
	}
	if idleTimeout <= 0 {
		idleTimeout = defaultConfig().IdleTimeout
	}
	ttl := cfg.TTL
	if stored, ok := parseDurationSecondsLabel(next["ttl_secs"]); ok {
		ttl = stored
	}
	if ttl <= 0 {
		ttl = defaultConfig().TTL
	}
	next["last_touched_at"] = leaseLabelTime(now)
	next["idle_timeout"] = durationSecondsLabel(idleTimeout)
	next["idle_timeout_secs"] = durationSecondsLabel(idleTimeout)
	next["ttl_secs"] = durationSecondsLabel(ttl)
	next["expires_at"] = leaseLabelTime(directLeaseExpiresAtFrom(createdAt, now, ttl, idleTimeout))
	return sanitizeProviderLabels(next)
}

func directLeaseExpiresAtFrom(createdAt, lastTouchedAt time.Time, ttl, idleTimeout time.Duration) time.Time {
	expiresAt := lastTouchedAt.Add(idleTimeout)
	if ttl > 0 {
		ttlExpiresAt := createdAt.Add(ttl)
		if ttlExpiresAt.Before(expiresAt) {
			expiresAt = ttlExpiresAt
		}
	}
	return expiresAt
}

func leaseLabelTime(t time.Time) string {
	return strconv.FormatInt(t.UTC().Unix(), 10)
}

func parseLeaseLabelTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Unix(seconds, 0).UTC(), true
	}
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func leaseLabelTimeDisplay(value string) string {
	t, ok := parseLeaseLabelTime(value)
	if !ok {
		return ""
	}
	return t.Format(time.RFC3339)
}

func durationSecondsLabel(duration time.Duration) string {
	if duration <= 0 {
		return ""
	}
	return strconv.FormatInt(int64(duration.Round(time.Second)/time.Second), 10)
}

func parseDurationSecondsLabel(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second, true
	}
	if duration, err := time.ParseDuration(value); err == nil && duration > 0 {
		return duration, true
	}
	return 0, false
}

func leaseLabelDurationDisplay(secondsValue, fallbackValue string) string {
	if duration, ok := parseDurationSecondsLabel(secondsValue); ok {
		return duration.String()
	}
	if duration, ok := parseDurationSecondsLabel(fallbackValue); ok {
		return duration.String()
	}
	return ""
}

func sanitizeProviderLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		out[key] = sanitizeProviderLabelValue(value)
	}
	return out
}

func sanitizeProviderLabelValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
		if b.Len() >= 63 {
			break
		}
	}
	out := strings.Trim(b.String(), "_.-")
	if out == "" {
		return "unknown"
	}
	return out
}
