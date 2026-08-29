package incus

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/lxc/incus/v7/shared/api"
	"golang.org/x/crypto/ssh"
)

var forkUserPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]*[$]?$`)

// Only the stopped clone is changed. Incus regenerates its own UUID and network
// identity; cloud-init's per-instance user module would append inherited keys.
func prepareForkIdentity(client instanceClient, cfg Config, inst api.Instance, publicKey string) error {
	if inst.IsActive() || inst.Type != "container" {
		return fmt.Errorf("fork identity replacement requires a stopped container")
	}
	if err := rejectAttachedDisks(inst); err != nil {
		return err
	}
	if !forkUserPattern.MatchString(cfg.SSHUser) {
		return fmt.Errorf("unsupported Incus fork SSH user")
	}
	port, err := strconv.Atoi(cfg.Incus.LaunchPort)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid Incus fork guest SSH port")
	}
	// Image templates run at first start before the guest does. Disable them on
	// the clone so they cannot overwrite the identity files installed below.
	if err := client.ClearTemplates(inst.Name); err != nil {
		return err
	}
	passwd, err := client.ReadFile(inst.Name, "/etc/passwd")
	if err != nil {
		return err
	}
	home := ""
	for _, line := range strings.Split(string(passwd), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) == 7 && fields[0] == cfg.SSHUser {
			home = fields[5]
		}
	}
	if !strings.HasPrefix(home, "/") || path.Clean(home) == "/" {
		return fmt.Errorf("source image is missing the configured SSH user's home")
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		return err
	}
	hostPublic, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		return err
	}
	machineID := strings.ReplaceAll(uuid.NewString(), "-", "") + "\n"
	sshConfig := fmt.Sprintf(`Port %d
HostKey /etc/ssh/ssh_host_ed25519_key
AuthorizedKeysFile /etc/ssh/crabbox_authorized_keys
AuthorizedKeysCommand none
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
PermitRootLogin prohibit-password
AllowUsers %s
UsePAM yes
PermitUserEnvironment no
PermitUserRC no
Subsystem sftp internal-sftp
`, port, cfg.SSHUser)
	files := []struct {
		path, content string
		mode          int
	}{
		{"/etc/cloud/cloud-init.disabled", "", 0644},
		{"/etc/ssh/crabbox_authorized_keys", strings.TrimSpace(publicKey) + "\n", 0644},
		{path.Join(home, ".ssh/authorized_keys"), strings.TrimSpace(publicKey) + "\n", 0644},
		{"/etc/ssh/ssh_host_ed25519_key", string(pem.EncodeToMemory(block)), 0600},
		{"/etc/ssh/ssh_host_ed25519_key.pub", string(ssh.MarshalAuthorizedKey(hostPublic)), 0644},
		{"/etc/ssh/sshd_config", sshConfig, 0644},
		{"/etc/machine-id", machineID, 0444},
		{"/var/lib/dbus/machine-id", machineID, 0444},
		{"/etc/hostname", inst.Name + "\n", 0644},
	}
	for _, file := range files {
		if err := client.WriteFile(inst.Name, file.path, []byte(file.content), file.mode); err != nil {
			return fmt.Errorf("replace fork file %s: %w", file.path, err)
		}
		observed, err := client.ReadFile(inst.Name, file.path)
		if err != nil {
			return err
		}
		if !bytes.Equal(observed, []byte(file.content)) {
			return fmt.Errorf("fork file %s did not retain replacement contents", file.path)
		}
	}
	return nil
}
