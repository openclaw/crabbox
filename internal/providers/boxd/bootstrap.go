package boxd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"golang.org/x/crypto/ssh/knownhosts"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"golang.org/x/crypto/ssh"
	"nhooyr.io/websocket"
)

const hostKeyMarker = "CRABBOX_BOXD_HOST_KEY "
const maxTerminalOutput = 256 << 10

// Only public keys enter this script. The terminal is authenticated by HTTPS;
// its host-key output is authoritative before the first SSH connection.
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
	// The interactive shell may echo this line before stty takes effect. Base64
	// keeps the output marker out of that echo, so it cannot forge completion.
	return "stty -echo; exec bash -c 'set -o pipefail; printf %s " + base64.StdEncoding.EncodeToString([]byte(script)) + " | base64 -d | sudo -n bash'\n", nil
}

func (c *consoleClient) bootstrap(ctx context.Context, id, publicKey string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	command, err := bootstrapCommand(publicKey)
	if err != nil {
		return "", err
	}
	route, err := machineRoute(id, "term")
	if err != nil {
		return "", err
	}
	u := *c.base
	u.Scheme, u.Path = "wss", route
	q := u.Query()
	q.Set("token", c.token)
	q.Set("cols", "120")
	q.Set("rows", "40")
	u.RawQuery = q.Encode()
	conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPClient: c.http})
	if err != nil {
		return "", terminalError(ctx, "connect")
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxTerminalOutput)
	if err := conn.Write(ctx, websocket.MessageBinary, []byte(command)); err != nil {
		return "", terminalError(ctx, "write")
	}
	var output strings.Builder
	total := 0
	for {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			return "", terminalError(ctx, "disconnected without completion")
		}
		total += len(data)
		if total > maxTerminalOutput {
			return "", core.Exit(5, "boxd terminal output exceeded limit")
		}
		if kind == websocket.MessageBinary {
			output.Write(data)
			continue
		}
		var event struct {
			Type string `json:"type"`
			Code *int   `json:"code"`
		}
		if json.Unmarshal(data, &event) != nil || event.Type != "exit" || event.Code == nil {
			return "", core.Exit(5, "invalid boxd terminal completion")
		}
		if *event.Code != 0 {
			return "", core.Exit(5, "boxd guest bootstrap failed (nonzero terminal exit)")
		}
		return terminalHostKey(output.String())
	}
}

func terminalError(ctx context.Context, phase string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// WebSocket errors can contain the JWT query, unsafe close reasons or bodies.
	return core.Exit(5, "boxd authenticated terminal %s", phase)
}

func terminalHostKey(output string) (string, error) {
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
