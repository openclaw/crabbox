package shared

import (
	"math"
	"time"
)

// SecondsWithGrace bounds both conversion and addition. Callers own timeout
// defaults, request payloads, and any provider-specific admission policy.
func SecondsWithGrace(seconds int64, grace time.Duration) (time.Duration, bool) {
	if seconds < 0 || grace < 0 || seconds > (math.MaxInt64-int64(grace))/int64(time.Second) {
		return 0, false
	}
	return time.Duration(seconds)*time.Second + grace, true
}
