package nebius

import (
	"fmt"
	"regexp"
	"strings"
)

var linuxUsernamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func renderNebiusCloudInit(user, publicKey string) (string, error) {
	user = strings.TrimSpace(user)
	publicKey = strings.TrimSpace(publicKey)
	if err := validateNebiusUser(user); err != nil {
		return "", err
	}
	if publicKey == "" {
		return "", validationError("ssh public key is required for nebius cloud-init")
	}
	return fmt.Sprintf(`#cloud-config
users:
  - name: %s
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    groups: [sudo]
    ssh_authorized_keys:
      - %s
ssh_pwauth: false
disable_root: true
`, user, publicKey), nil
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
