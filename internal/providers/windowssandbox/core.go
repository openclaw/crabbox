package windowssandbox

import (
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName      = "windows-sandbox"
	targetWindows     = core.TargetWindows
	windowsModeNormal = core.WindowsModeNormal
)

func durationSecondsCeil(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}
	return int((duration + time.Second - 1) / time.Second)
}
