package linode

import (
	"encoding/base64"

	core "github.com/openclaw/crabbox/internal/cli"
)

func linodeUserData(cfg core.Config, publicKey string) string {
	return base64.StdEncoding.EncodeToString([]byte(core.CloudInitUserData(cfg, publicKey)))
}
