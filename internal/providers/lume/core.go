package lume

import (
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName                 = "lume"
	targetMacOS                  = core.TargetMacOS
	sshPort                      = "22"
	defaultStartupObserveTimeout = 2 * time.Second
	defaultStopObserveTimeout    = 30 * time.Second
	defaultStopPollInterval      = 500 * time.Millisecond
	defaultGuestIdentityTimeout  = 2 * time.Minute
)

func directLeaseLabels(cfg core.Config, leaseID, slug string, keep bool, now time.Time) map[string]string {
	return core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
}

func resolveLeaseClaimForProvider(identifier string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(identifier, providerName)
}
