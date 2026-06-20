package nebius

import (
	"regexp"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

var linuxUsernamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func renderNebiusCloudInit(cfg Config, publicKey string) (string, error) {
	user := strings.TrimSpace(cfg.SSHUser)
	if user == "" {
		user = strings.TrimSpace(cfg.Nebius.User)
	}
	publicKey = strings.TrimSpace(publicKey)
	if err := validateNebiusUser(user); err != nil {
		return "", err
	}
	if publicKey == "" {
		return "", validationError("ssh public key is required for nebius cloud-init")
	}
	cfg.SSHUser = user
	return core.CloudInitUserData(cfg, publicKey) + "ssh_pwauth: false\ndisable_root: true\n", nil
}

func validateNebiusUser(user string) error {
	user = strings.TrimSpace(user)
	switch strings.ToLower(user) {
	case "", "root", "admin":
		return validationError("nebius.user must be a non-reserved Linux username")
	}
	if !linuxUsernamePattern.MatchString(user) {
		return validationError("nebius.user must match Linux username pattern %s", linuxUsernamePattern.String())
	}
	return nil
}
