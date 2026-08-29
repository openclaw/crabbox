package gcp

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) ClaimScope(cfg core.Config) string {
	return gcpClaimScope(cfg)
}
