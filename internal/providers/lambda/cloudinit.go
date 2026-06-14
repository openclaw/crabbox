package lambda

import core "github.com/openclaw/crabbox/internal/cli"

func lambdaUserData(cfg core.Config, publicKey string) string {
	return core.CloudInitUserData(cfg, publicKey)
}
