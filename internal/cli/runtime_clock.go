package cli

import "time"

// ClockNow returns the injected time, or the system time when clock is nil.
func ClockNow(clock Clock) time.Time {
	if clock != nil {
		return clock.Now()
	}
	return time.Now()
}
