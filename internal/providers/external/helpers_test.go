package external

import core "github.com/openclaw/crabbox/internal/cli"

func lifecycleSSH(cfg core.ExternalSSHConnectionConfig, templateCtx lifecycleTemplateContext) (protocolSSH, error) {
	return lifecycleSSHForTarget(cfg, templateCtx, core.TargetLinux, core.WindowsModeNormal)
}
