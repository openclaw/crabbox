package tart

import (
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName                 = "tart"
	targetMacOS                  = core.TargetMacOS
	sshPort                      = "22"
	defaultStartupObserveTimeout = 2 * time.Second
)
