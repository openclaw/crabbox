package phala

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) ClaimScope(cfg core.Config) string {
	if node := strings.TrimSpace(cfg.Phala.NodeID); node != "" {
		return "node:" + node
	}
	return ""
}
