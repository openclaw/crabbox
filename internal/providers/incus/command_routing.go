package incus

import (
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	if core.DeleteOnReleaseExplicit(cfg, "incus") {
		args = append(args, fmt.Sprintf("--incus-delete-on-release=%t", cfg.Incus.DeleteOnRelease))
	}
	if strings.TrimSpace(cfg.Incus.Remote) != "" {
		args = append(args, "--incus-remote", cfg.Incus.Remote)
	}
	if strings.TrimSpace(cfg.Incus.Project) != "" {
		args = append(args, "--incus-project", cfg.Incus.Project)
	}
	if strings.TrimSpace(cfg.Incus.Address) != "" {
		args = append(args, "--incus-address", core.RoutingSafeURL(cfg.Incus.Address))
	}
	if strings.TrimSpace(cfg.Incus.Socket) != "" {
		args = append(args, "--incus-socket", cfg.Incus.Socket)
	}
	if strings.TrimSpace(cfg.Incus.User) != "" {
		args = append(args, "--incus-user", cfg.Incus.User)
	}
	if strings.TrimSpace(cfg.Incus.WorkRoot) != "" {
		args = append(args, "--incus-work-root", cfg.Incus.WorkRoot)
	}
	if strings.TrimSpace(cfg.Incus.LaunchPort) != "" {
		args = append(args, "--incus-launch-port", cfg.Incus.LaunchPort)
	}
	if strings.TrimSpace(cfg.Incus.ProxyListenHost) != "" {
		args = append(args, "--incus-proxy-listen-host", cfg.Incus.ProxyListenHost)
	}
	if strings.TrimSpace(cfg.Incus.ProxyListenPort) != "" {
		args = append(args, "--incus-proxy-listen-port", cfg.Incus.ProxyListenPort)
	}
	if strings.TrimSpace(cfg.Incus.ProxyDevice) != "" {
		args = append(args, "--incus-proxy-device", cfg.Incus.ProxyDevice)
	}
	if strings.TrimSpace(cfg.Incus.TLSServerCert) != "" {
		args = append(args, "--incus-tls-server-cert", cfg.Incus.TLSServerCert)
	}
	args = append(args, fmt.Sprintf("--incus-insecure-tls=%t", cfg.Incus.InsecureTLS))
	return core.CommandRouting{Args: args}
}
