package shared

import (
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

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
	deadline := lastUsed.Add(time.Duration(claim.IdleTimeoutSeconds) * time.Second)
	if now.Before(deadline) {
		return false, "idle timeout not reached"
	}
	return true, "idle timeout"
}
