package morph

import (
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	if strings.TrimSpace(cfg.Morph.APIURL) != "" {
		endpoint := core.RoutingSafeURL(cfg.Morph.APIURL)
		originalScope, originalErr := normalizeMorphAPIURL(cfg.Morph.APIURL)
		routedScope, _ := normalizeMorphAPIURL(endpoint)
		// Existing scopes can include userinfo; keep that endpoint in private
		// config instead of emitting a redacted override with a different scope.
		if originalErr != nil || originalScope == routedScope {
			args = append(args, "--morph-api-url", endpoint)
		}
	}
	if core.DeleteOnReleaseExplicit(cfg, "morph") {
		args = append(args, fmt.Sprintf("--morph-delete-on-release=%t", cfg.Morph.DeleteOnRelease))
	}
	if strings.TrimSpace(cfg.Morph.SSHGatewayHost) != "" {
		args = append(args, "--morph-ssh-gateway-host", cfg.Morph.SSHGatewayHost)
	}
	if strings.TrimSpace(cfg.Morph.WorkRoot) != "" {
		args = append(args, "--morph-work-root", cfg.Morph.WorkRoot)
	}
	args = append(args, fmt.Sprintf("--morph-wake-on-ssh=%t", cfg.Morph.WakeOnSSH))
	return core.CommandRouting{Args: args}
}
