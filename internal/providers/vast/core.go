package vast

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName = "vast"
)

func markReleaseActionExplicit(cfg *core.Config) {
	core.MarkDeleteOnReleaseExplicit(cfg, providerName)
}
