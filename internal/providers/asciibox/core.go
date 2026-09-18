package asciibox

import (
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName = "ascii-box"
	targetLinux  = core.TargetLinux

	networkPublic = core.NetworkPublic
)

func now(rt core.Runtime) time.Time {
	if rt.Clock != nil {
		return rt.Clock.Now()
	}
	return time.Now()
}
