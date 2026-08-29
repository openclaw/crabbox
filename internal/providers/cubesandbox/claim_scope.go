package cubesandbox

import (
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func (Provider) ClaimScope(cfg core.Config) string {
	if endpoint := shared.NormalizedSandboxClaimEndpoint(cfg.CubeSandbox.APIURL); endpoint != "" {
		return "endpoint:" + endpoint
	}
	return ""
}
