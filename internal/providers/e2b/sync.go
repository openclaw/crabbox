package e2b

import (
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func workspaceForConfig(cfg core.Config, rt core.Runtime) shared.EnvdWorkspace {
	return shared.EnvdWorkspace{
		Provider: e2bProvider, Config: cfg, Runtime: rt,
		Workdir: cfg.E2B.Workdir, User: cfg.E2B.User, DefaultUser: "",
		RemoteArchivePrefix: "crabbox-",
	}
}
