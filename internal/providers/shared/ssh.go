package shared

import (
	"os"

	core "github.com/openclaw/crabbox/internal/cli"
)

func UseStoredTestboxKey(target *core.SSHTarget, leaseID string) {
	if keyPath, err := core.TestboxKeyPath(leaseID); err == nil {
		if _, statErr := os.Stat(keyPath); statErr == nil {
			target.Key = keyPath
		}
	}
}
