package shared

import (
	"math"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// PositiveIdleDuration converts persisted idle seconds without overflow. Invalid
// values cannot authorize expiry; callers retain their timing and diagnostic policy.
func PositiveIdleDuration(seconds int) (time.Duration, bool) {
	raw := int64(seconds)
	if raw <= 0 || raw > math.MaxInt64/int64(time.Second) {
		return 0, false
	}
	return time.Duration(raw) * time.Second, true
}

// ClaimIdleCleanupDue evaluates only the local claim's idle deadline. Provider
// TTL policy, ownership checks, and authorization to delete remain with callers.
func ClaimIdleCleanupDue(claim core.LeaseClaim, now time.Time) (bool, string) {
	if claim.IdleTimeoutSeconds <= 0 {
		return false, "idle timeout disabled"
	}
	lastUsed, err := time.Parse(time.RFC3339, strings.TrimSpace(claim.LastUsedAt))
	if err != nil {
		return false, "invalid last-used time"
	}
	idle, valid := PositiveIdleDuration(claim.IdleTimeoutSeconds)
	if !valid {
		return false, "invalid idle timeout"
	}
	deadline := lastUsed.Add(idle)
	if now.Before(deadline) {
		return false, "idle timeout not reached"
	}
	return true, "idle timeout"
}
