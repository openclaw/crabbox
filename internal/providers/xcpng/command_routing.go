package xcpng

import (
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	if strings.TrimSpace(cfg.XCPNg.APIURL) != "" {
		args = append(args, "--xcp-ng-api-url", core.RoutingSafeURL(cfg.XCPNg.APIURL))
	}
	if strings.TrimSpace(cfg.XCPNg.Username) != "" {
		args = append(args, "--xcp-ng-username", cfg.XCPNg.Username)
	}
	if strings.TrimSpace(cfg.XCPNg.Template) != "" {
		args = append(args, "--xcp-ng-template", cfg.XCPNg.Template)
	}
	if strings.TrimSpace(cfg.XCPNg.TemplateUUID) != "" {
		args = append(args, "--xcp-ng-template-uuid", cfg.XCPNg.TemplateUUID)
	}
	if strings.TrimSpace(cfg.XCPNg.SR) != "" {
		args = append(args, "--xcp-ng-sr", cfg.XCPNg.SR)
	}
	if strings.TrimSpace(cfg.XCPNg.SRUUID) != "" {
		args = append(args, "--xcp-ng-sr-uuid", cfg.XCPNg.SRUUID)
	}
	if strings.TrimSpace(cfg.XCPNg.Network) != "" {
		args = append(args, "--xcp-ng-network", cfg.XCPNg.Network)
	}
	if strings.TrimSpace(cfg.XCPNg.NetworkUUID) != "" {
		args = append(args, "--xcp-ng-network-uuid", cfg.XCPNg.NetworkUUID)
	}
	if strings.TrimSpace(cfg.XCPNg.Host) != "" {
		args = append(args, "--xcp-ng-host", cfg.XCPNg.Host)
	}
	if strings.TrimSpace(cfg.XCPNg.User) != "" {
		args = append(args, "--xcp-ng-user", cfg.XCPNg.User)
	}
	if strings.TrimSpace(cfg.XCPNg.WorkRoot) != "" {
		args = append(args, "--xcp-ng-work-root", cfg.XCPNg.WorkRoot)
	}
	if cfg.XCPNg.InsecureTLS {
		args = append(args, "--xcp-ng-insecure-tls")
	} else {
		args = append(args, fmt.Sprintf("--xcp-ng-insecure-tls=%t", cfg.XCPNg.InsecureTLS))
	}
	return core.CommandRouting{Args: args}
}
