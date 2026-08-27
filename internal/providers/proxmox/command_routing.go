package proxmox

import (
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	if strings.TrimSpace(cfg.Proxmox.APIURL) != "" {
		endpoint := core.RoutingSafeURL(cfg.Proxmox.APIURL)
		// Schemeless legacy scopes are opaque: do not replace their private
		// configured endpoint with a redacted, differently scoped value.
		if normalizedProxmoxClaimEndpoint(endpoint) == normalizedProxmoxClaimEndpoint(cfg.Proxmox.APIURL) {
			args = append(args, "--proxmox-api-url", endpoint)
		}
	}
	if strings.TrimSpace(cfg.Proxmox.Node) != "" {
		args = append(args, "--proxmox-node", cfg.Proxmox.Node)
	}
	if cfg.Proxmox.InsecureTLS {
		args = append(args, "--proxmox-insecure-tls")
	} else {
		args = append(args, fmt.Sprintf("--proxmox-insecure-tls=%t", cfg.Proxmox.InsecureTLS))
	}
	if strings.TrimSpace(cfg.Proxmox.User) != "" {
		args = append(args, "--proxmox-user", cfg.Proxmox.User)
	}
	if strings.TrimSpace(cfg.Proxmox.WorkRoot) != "" {
		args = append(args, "--proxmox-work-root", cfg.Proxmox.WorkRoot)
	}
	return core.CommandRouting{Args: args}
}
