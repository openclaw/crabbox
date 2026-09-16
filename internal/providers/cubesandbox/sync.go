package cubesandbox

import (
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func workspaceForConfig(cfg core.Config, rt core.Runtime) shared.EnvdWorkspace {
	return shared.EnvdWorkspace{
		Provider: providerName, Config: cfg, Runtime: rt,
		Workdir: cfg.CubeSandbox.Workdir, User: cfg.CubeSandbox.User, DefaultUser: "root",
		RemoteArchivePrefix: "crabbox-cubesandbox-sync-",
	}
}
