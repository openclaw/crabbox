package shared

import "math"

// MiBToBytes converts a nonnegative size without overflowing a signed byte count.
// Provider minimums, defaults, and practical resource limits stay with callers.
func MiBToBytes(mib int64) (int64, bool) {
	const bytesPerMiB = 1 << 20
	if mib < 0 || mib > math.MaxInt64/bytesPerMiB {
		return 0, false
	}
	return mib * bytesPerMiB, true
}
