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
	for _, tc := range []struct {
		name, socket, failure, initial, wantListen string
		restart                                    bool
	}{
		{"active socket", "active", "", "22", "2222 22", true},
		{"service only", "inactive", "", "22", "2222 22", true},
		{"socket already listening", "active", "", "2222 22", "2222 22", false},
		{"service already listening", "inactive", "", "2222 22", "2222 22", false},
		{"missing fallback", "active", "", "2222", "2222 22", true},
		{"extra listener", "active", "", "2222 22 2200", "2222 22 2200", false},
		{"unrelated listener", "active", "unrelated", "2222 22", "2222 22", true},
		{"listener probe fails", "active", "ss", "2222 22", "2222 22", true},
		{"config probe fails", "active", "sshd", "2222 22", "2222 22", true},
		{"socket restart fails", "active", "restart", "22", "22", true},
		{"service restart fails", "inactive", "restart", "22", "22", true},
		{"non-systemd host", "inactive", "unavailable", "22", "22", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prelude := `set -eu
configured_ports='2222 22'
generated_ports=22
listening_ports="$INITIAL"
restarted=false
sshd() {
 [ "$FAILURE" != sshd ] || return 1
 for port in $configured_ports; do printf 'port %s\n' "$port"; done
}
ss() {
 [ "$FAILURE" != ss ] || return 1
 owner=sshd
 [ "$SOCKET" != active ] || owner=systemd
 [ "$FAILURE" != unrelated ] || owner=httpd
 for port in $listening_ports; do
  printf 'LISTEN 0 128 [::]:%s [::]:* users:(("%s",pid=1,fd=3))\n' "$port" "$owner"
 done
}
systemctl() {
 printf 'systemctl %s\n' "$*" >&2
 [ "$FAILURE" != unavailable ] || return 127
 case "$*" in
  daemon-reload) generated_ports="$configured_ports" ;;
  'is-active --quiet ssh.socket') [ "$SOCKET" = active ] ;;
  'restart ssh.socket')
   restarted=true
   [ "$FAILURE" != restart ] || return 1
   listening_ports="$generated_ports" ;;
  'restart ssh'|'restart ssh.service')
   restarted=true
   [ "$FAILURE" != restart ] || return 1
   if [ "$SOCKET" != active ]; then listening_ports="$configured_ports"; fi ;;
  *) return 99 ;;
 esac
}
timeout() { [ "$1" = 30s ] || return 98; shift; "$@"; }
`
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			activation := strings.ReplaceAll(sharedLinuxSSHRestart(), "/usr/sbin/sshd", "sshd")
			cmd := exec.CommandContext(ctx, "bash", "-c", prelude+activation+"\nprintf '%s|%s\\n' \"$listening_ports\" \"$restarted\"\n")
			cmd.Env = []string{"PATH=/usr/bin:/bin", "SOCKET=" + tc.socket, "FAILURE=" + tc.failure, "INITIAL=" + tc.initial}
			cmd.WaitDelay = time.Second
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("SSH activation failed: %v\n%s", err, output)
			}
			restart := "false"
			if tc.restart {
				restart = "true"
			}
			if !strings.HasSuffix(string(output), tc.wantListen+"|"+restart+"\n") {
				t.Fatalf("want listening %q, restart %s:\n%s", tc.wantListen, restart, output)
			}
			if !tc.restart && tc.failure == "" && strings.Contains(string(output), "systemctl") {
				t.Fatalf("matching listeners must not reload or restart systemd:\n%s", output)
			}
		})
	}
}

func TestCloudInitBatchesServiceEnablement(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		wantEnable := ""
		enabledResult := "0"
		if !enabled {
			enabledResult = "1"
			wantEnable = "enable --no-reload ssh\n"
		}
		prelude := "set -eu\nsystemctl() {\nif [ \"$*\" = 'is-enabled --quiet ssh' ]; then return " + enabledResult + "; fi\nprintf '%s\\n' \"$*\"\n}\n"
		out, err := exec.Command("bash", "-c", prelude+sharedLinuxBootstrapActivate()).CombinedOutput()
		if err != nil {
			t.Fatalf("activation: %v: %s", err, out)
		}
		want := wantEnable + "enable --no-reload crabbox-workspace-ready.service\ndaemon-reload\nstart --no-block crabbox-workspace-ready.service\n"
		if string(out) != want {
			t.Fatalf("enabled=%v: got %s; want %s", enabled, out, want)
		}
	}
}
