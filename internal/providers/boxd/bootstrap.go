package boxd

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/boxd/boxdapi"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const hostKeyMarker = "CRABBOX_BOXD_HOST_KEY "
const maxExecOutput = 256 << 10

// Only public keys enter this script. The exec stream is authenticated gRPC
// over TLS; its host-key output is authoritative before the first SSH
// connection.
func bootstrapCommand(publicKey string) (string, error) {
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil || len(options) != 0 || len(strings.TrimSpace(string(rest))) != 0 {
		return "", core.Exit(2, "invalid Boxd lease public key")
	}
	key64 := base64.StdEncoding.EncodeToString(ssh.MarshalAuthorizedKey(key))
	script := `set -eu
command -v sshd >/dev/null
command -v ssh-keygen >/dev/null
test -f /etc/ssh/ssh_host_ed25519_key
install -d -o root -g root -m 0755 /etc/crabbox-ssh /run/sshd
printf '%s' '` + key64 + `' | base64 -d > /etc/crabbox-ssh/authorized_keys
chown root:root /etc/crabbox-ssh/authorized_keys
chmod 0644 /etc/crabbox-ssh/authorized_keys
cat > /etc/crabbox-ssh/sshd_config <<'CONFIG'
Port 2222
ListenAddress 0.0.0.0
HostKey /etc/ssh/ssh_host_ed25519_key
PidFile /run/crabbox-sshd.pid
AuthorizedKeysFile /etc/crabbox-ssh/authorized_keys
AuthorizedKeysCommand none
TrustedUserCAKeys none
PasswordAuthentication no
KbdInteractiveAuthentication no
AuthenticationMethods publickey
PubkeyAuthentication yes
PermitRootLogin no
PermitEmptyPasswords no
AllowUsers boxd
UsePAM yes
StrictModes yes
AllowTcpForwarding yes
Subsystem sftp internal-sftp
CONFIG
chown root:root /etc/crabbox-ssh/sshd_config
chmod 0600 /etc/crabbox-ssh/sshd_config
/usr/sbin/sshd -t -f /etc/crabbox-ssh/sshd_config
if ! test -s /run/crabbox-sshd.pid || ! kill -0 "$(cat /run/crabbox-sshd.pid)" 2>/dev/null; then
 /usr/sbin/sshd -f /etc/crabbox-ssh/sshd_config
fi
host_key="$(ssh-keygen -y -f /etc/ssh/ssh_host_ed25519_key)"
printf '\n` + hostKeyMarker + `%s\n' "$host_key"
`
	// The exec runs without a PTY, so nothing echoes the command line back;
	// base64 additionally keeps the output marker out of the command itself,
	// so a transcript of the sent command cannot forge completion.
	return "bash -c 'set -o pipefail; printf %s " + base64.StdEncoding.EncodeToString([]byte(script)) + " | base64 -d | sudo -n bash'", nil
}

// bootstrap installs the per-lease key and dedicated sshd through the
// authenticated gRPC exec stream and returns the guest's SSH host key. A
// nonzero exit code on any chunk fails the bootstrap; a clean end of stream
// without one is completion with exit code zero, and the host-key marker must
// then be present on stdout.
func (c *apiClient) bootstrap(ctx context.Context, id, publicKey string) (string, error) {
	if err := validateMachineID(id); err != nil {
		return "", err
	}
	command, err := bootstrapCommand(publicKey)
	if err != nil {
		return "", err
	}
	ctx, cancel, err := c.authed(ctx, 2*time.Minute)
	if err != nil {
		return "", err
	}
	defer cancel()
	stream, err := c.api.Exec(ctx)
	if err != nil {
		return "", execError(ctx, "connect")
	}
	if err := stream.Send(&boxdapi.ExecChunk{VmId: id, Command: command}); err != nil {
		return "", execError(ctx, "write")
	}
	if err := stream.CloseSend(); err != nil {
		return "", execError(ctx, "write")
	}
	var stdout strings.Builder
	total, exitCode := 0, 0
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", execError(ctx, "disconnected without completion")
		}
		total += len(chunk.GetData())
		if total > maxExecOutput {
			return "", core.Exit(5, "boxd exec output exceeded limit")
		}
		if chunk.GetExitCode() != 0 {
			exitCode = int(chunk.GetExitCode())
		}
		if len(chunk.GetData()) > 0 && !chunk.GetIsStderr() {
			stdout.Write(chunk.GetData())
		}
	}
	if exitCode != 0 {
		return "", core.Exit(5, "boxd guest bootstrap failed (nonzero exit)")
	}
	return execHostKey(stdout.String())
}

func execError(ctx context.Context, phase string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Stream errors can carry vendor status text or partial output. Withhold
	// them from diagnostics.
	return core.Exit(5, "boxd authenticated exec %s", phase)
}

func execHostKey(output string) (string, error) {
	var result string
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(line, hostKeyMarker) {
			continue
		}
		if result != "" {
			return "", core.Exit(5, "duplicate boxd SSH host-key output")
		}
		key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(strings.TrimPrefix(line, hostKeyMarker)))
		if err != nil || len(options) != 0 || len(strings.TrimSpace(string(rest))) != 0 || key.Type() != ssh.KeyAlgoED25519 {
			return "", core.Exit(5, "invalid boxd SSH host-key output")
		}
		result = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	}
	if result == "" {
		return "", core.Exit(5, "boxd bootstrap completed without an authoritative SSH host key")
	}
	return result, nil
}

// UseLeaseKnownHosts creates the protected lease directory. Publish atomically
// within it so an existing file (including a symlink) is never followed.
func pinHostKey(target core.SSHTarget) error {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(target.SSHHostKey))
	if err != nil {
		return core.Exit(5, "invalid boxd SSH host key")
	}
	file, err := os.CreateTemp(filepath.Dir(target.KnownHostsFile), ".known-hosts-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.WriteString(knownhosts.Line([]string{net.JoinHostPort(target.Host, target.Port)}, key) + "\n")
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return os.Rename(file.Name(), target.KnownHostsFile)
}
