package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"path"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
)

const GuestBootstrapPublicKeyV1 = "ssh-public-key-v1"
const AWSGuestRolelessBootstrapException = "aws-roleless-imdsv2-public-bootstrap-v1"

// GuestPublicKeyUserData deliberately accepts no configuration, environment,
// hooks, provider credentials or private key. It is trusted base initialization,
// not workload bootstrap; workload material stays gated on provider inspection.
func GuestPublicKeyUserData(publicKey, user, workRoot string) (string, error) {
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(publicKey)))
	if err != nil || len(strings.TrimSpace(string(rest))) != 0 {
		return "", errors.New("guest bootstrap requires one valid public SSH key")
	}
	if !regexp.MustCompile("^[a-z_][a-z0-9_-]{0,31}$").MatchString(user) || !path.IsAbs(workRoot) || path.Clean(workRoot) != workRoot || strings.ContainsAny(workRoot, "\x00\r\n") {
		return "", errors.New("invalid guest bootstrap user/work root")
	}
	normalized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	data := map[string]any{
		"ssh_pwauth": false, "disable_root": true,
		"users":       []any{map[string]any{"name": user, "groups": []string{"sudo"}, "shell": "/bin/bash", "sudo": []string{"ALL=(ALL) NOPASSWD:ALL"}, "lock_passwd": true, "ssh_authorized_keys": []string{normalized}}},
		"write_files": []any{map[string]any{"path": "/usr/local/bin/crabbox-ready", "permissions": "0755", "content": "#!/bin/sh\nset -eu\ntest -f /var/lib/crabbox/guest-public-key-ready\n"}},
		"runcmd":      [][]string{{"install", "-d", "-m", "0755", "-o", user, "-g", user, workRoot}, {"install", "-d", "-m", "0755", "/var/lib/crabbox"}, {"/bin/sh", "-c", "{ printf 'CRABBOX_GUEST_HOST_KEY_V1 '; cut -d ' ' -f 1,2 /etc/ssh/ssh_host_ed25519_key.pub; } > /dev/console"}, {"touch", "/var/lib/crabbox/guest-public-key-ready"}},
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	dataBytes := append([]byte("#cloud-config\n"), encoded...)
	if len(dataBytes) > awsEC2UserDataLimit {
		return "", errors.New("guest public-key bootstrap exceeds EC2 userdata limit")
	}
	return base64.StdEncoding.EncodeToString(dataBytes), nil
}

func awsGuestBootstrapPolicy(policy GuestNetworkPolicy) error {
	expected := []string{AWSGuestNetworkPlatformServices}
	if policy.BootstrapMode == GuestBootstrapPublicKeyV1 {
		expected = append(expected, AWSGuestRolelessBootstrapException)
	} else if policy.BootstrapMode != "" {
		return errors.New("unknown guest bootstrap mode")
	}
	if len(policy.PlatformExceptions) != len(expected) {
		return errors.New("operator policy must explicitly acknowledge AWS platform/bootstrap exceptions")
	}
	for _, name := range expected {
		found := false
		for _, got := range policy.PlatformExceptions {
			found = found || got == name
		}
		if !found {
			return errors.New("operator policy must explicitly acknowledge AWS platform/bootstrap exceptions")
		}
	}
	return nil
}
