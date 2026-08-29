package ssh

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, request core.CommandRoutingRequest) core.CommandRouting {
	cfg.Static.Host = firstRoutingValue(cfg.Static.Host, request.Target.Host)
	cfg.Static.User = firstRoutingValue(cfg.Static.User, request.Target.User)
	cfg.Static.Port = firstRoutingValue(cfg.Static.Port, request.Target.Port)
	var args []string
	for _, field := range []struct{ flag, value string }{
		{"--static-host", cfg.Static.Host}, {"--static-user", cfg.Static.User},
		{"--static-port", cfg.Static.Port}, {"--static-work-root", cfg.Static.WorkRoot},
	} {
		if strings.TrimSpace(field.value) != "" {
			args = append(args, field.flag, field.value)
		}
	}
	return core.CommandRouting{Args: args}
}

func firstRoutingValue(configured, resolved string) string {
	if strings.TrimSpace(configured) != "" {
		return configured
	}
	return resolved
}
