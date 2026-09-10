//go:build !windows

package cli

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCloudInitReloadsSSHListenerWithoutPackageInstallation(t *testing.T) {
	// Readiness may skip APT on prepared images, so SSH cannot depend on the
	// openssh-server postinst to reload and restart the socket for new ports.
	script := cloudInit(baseConfig(), "ssh-ed25519 fixture")
	_, activation, found := strings.Cut(script, "    systemctl enable ssh || true\n")
	if !found {
		t.Fatal("missing SSH activation after baseline readiness")
	}
	activation, _, found = strings.Cut(activation, "    touch /var/lib/crabbox/bootstrapped")
	if !found {
		t.Fatal("missing bootstrap completion after SSH activation")
	}
	for _, tc := range []struct {
		name       string
		socket     string
		failure    string
		wantListen string
	}{
		{"active socket", "active", "", "2222 22"},
		{"service only", "inactive", "", "2222 22"},
		{"socket restart fails", "active", "restart", "22"},
		{"service restart fails", "inactive", "restart", "22"},
		{"non-systemd host", "inactive", "unavailable", "22"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Ubuntu's generator changes the loaded socket on daemon-reload;
			// service restarts inherit live descriptors until the socket restarts.
			prelude := `set -eu
configured_ports='2222 22'
generated_ports=22
listening_ports=22
systemctl() {
  printf 'systemctl %s\n' "$*" >&2
  [ "$FAILURE" != unavailable ] || return 127
  case "$*" in
    daemon-reload) generated_ports="$configured_ports" ;;
    'is-active --quiet ssh.socket') [ "$SOCKET" = active ] ;;
    'restart ssh.socket')
      [ "$FAILURE" != restart ] || return 1
      listening_ports="$generated_ports" ;;
    'restart ssh'|'restart ssh.service')
      [ "$FAILURE" != restart ] || return 1
      if [ "$SOCKET" != active ]; then listening_ports="$configured_ports"; fi ;;
    *) return 99 ;;
  esac
}
timeout() { [ "$1" = 30s ] || return 98; shift; "$@"; }
`
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-c", prelude+activation+"\nprintf '%s\\n' \"$listening_ports\"\n")
			cmd.Env = []string{"PATH=/usr/bin:/bin", "SOCKET=" + tc.socket, "FAILURE=" + tc.failure}
			cmd.WaitDelay = time.Second
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("SSH activation failed: %v\n%s", err, output)
			}
			if !strings.HasSuffix(string(output), tc.wantListen+"\n") {
				t.Fatalf("listening ports must be %q after activation:\n%s", tc.wantListen, output)
			}
		})
	}
}
