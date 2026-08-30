//go:build !windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceRsyncIPv6UsesResolvedSSHTransport(t *testing.T) {
	rsyncPath, err := exec.LookPath("rsync")
	if err != nil {
		t.Skip("rsync is required for the real operand parser probe")
	}
	clients := map[string]string{"path": rsyncPath}
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("ssh is required for the config resolution probe")
	}
	if _, err := os.Stat("/usr/bin/rsync"); err == nil {
		clients["system"] = "/usr/bin/rsync"
	}
	for name, client := range clients {
		t.Run(name, func(t *testing.T) {
			for _, host := range []string{"2001:db8::1", "192.0.2.1", "runner.example"} {
				t.Run(host, func(t *testing.T) {
					dir := t.TempDir()
					t.Setenv("HOME", dir)
					t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
					t.Setenv("CRABBOX_TRANSFER_PROBE", dir)
					for name, script := range map[string]string{
						"rsync": "#!/bin/sh\nexec " + shellQuote(client) + " \"$@\"\n",
						"ssh": `#!/bin/sh
printf '%s\n' "$@" > "$CRABBOX_TRANSFER_PROBE/argv"
while [ "$#" -gt 0 ]; do
  if [ "$1" = -F ]; then
    shift
    printf '%s' "$1" > "$CRABBOX_TRANSFER_PROBE/config-path"
    /bin/cp "$1" "$CRABBOX_TRANSFER_PROBE/config"
  fi
  shift
done
exit 23
`,
					} {
						if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
							t.Fatal(err)
						}
					}
					target := SSHTarget{Host: host, User: "fixture-secret-user", AuthSecret: true, Port: "2222"}
					if err := rsync(context.Background(), target, dir, "/work", nil, io.Discard, io.Discard, rsyncOptions{}); err == nil {
						t.Fatal("inert SSH must reject the transfer without connecting")
					}
					args, err := os.ReadFile(filepath.Join(dir, "argv"))
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(args), "\n"+sshTransportHostAlias+"\n") || !strings.Contains(string(args), "\n/work/\n") {
						t.Fatalf("rsync did not preserve the alias and remote path: %q", args)
					}
					if strings.Contains(string(args), host) || strings.Contains(string(args), target.User) {
						t.Fatal("resolved endpoint leaked into SSH argv")
					}
					config, err := exec.Command(sshPath, "-G", "-F", filepath.Join(dir, "config"), sshTransportHostAlias).Output()
					if err != nil {
						t.Fatal(err)
					}
					for _, want := range []string{"hostname " + host, "user " + target.User, "port 2222"} {
						if !strings.Contains(string(config), want) {
							t.Fatalf("private config missing %q", want)
						}
					}
					configPath, err := os.ReadFile(filepath.Join(dir, "config-path"))
					if err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(filepath.Dir(string(configPath))); !os.IsNotExist(err) {
						t.Fatalf("private transport directory survived failed transfer: %v", err)
					}
				})
			}
		})
	}
}

func TestSSHTransferIPv6SessionLifetimeAndEnvironment(t *testing.T) {
	for _, transfer := range []string{"rsync", "scp"} {
		for _, mode := range []string{"success", "failure", "cancel", "timeout"} {
			t.Run(transfer+"/"+mode, func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("HOME", dir)
				t.Setenv("TMPDIR", dir)
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				t.Setenv("CRABBOX_TRANSFER_PROBE", dir)
				t.Setenv("CRABBOX_TRANSFER_MODE", mode)
				t.Setenv("CRABBOX_FIXTURE_DENIED", "ambient-sentinel")
				t.Setenv("CRABBOX_FIXTURE_OVERRIDE", "ambient-sentinel")
				script := `#!/bin/sh
set -eu
printf '%s\n' "$@" > "$CRABBOX_TRANSFER_PROBE/argv"
printf '%s\n%s\n' "${CRABBOX_FIXTURE_DENIED+x}" "$CRABBOX_FIXTURE_OVERRIDE" > "$CRABBOX_TRANSFER_PROBE/environment"
for config in "$TMPDIR"/crabbox-ssh-transport-*/ssh_config; do
  [ -f "$config" ] || exit 24
  printf '%s' "$config" > "$CRABBOX_TRANSFER_PROBE/config-path"
  /bin/cp "$config" "$CRABBOX_TRANSFER_PROBE/config"
done
case "$CRABBOX_TRANSFER_MODE" in
  success) /bin/cat > "$CRABBOX_TRANSFER_PROBE/stdin"; exit 0 ;;
  failure) exit 23 ;;
  *) sleep 30 & child=$!; printf '%s' "$child" > "$CRABBOX_TRANSFER_PROBE/child-pid"; wait "$child" ;;
esac
`
				if err := os.WriteFile(filepath.Join(dir, transfer), []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				target := SSHTarget{
					Host: "2001:db8::2", User: "fixture-secret-user", AuthSecret: true, Port: "2222",
					Key: filepath.Join(dir, "identity"), CertificateFile: filepath.Join(dir, "identity-cert.pub"),
					ChildEnvDenylist: []string{"CRABBOX_FIXTURE_DENIED"},
					ChildEnv:         map[string]string{"CRABBOX_FIXTURE_OVERRIDE": "target-sentinel"},
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				opts := rsyncOptions{UseFilesFrom: true, FilesFrom: []byte("fixture.txt\x00"), Checksum: true}
				if mode == "timeout" {
					if transfer == "rsync" {
						opts.Timeout = time.Second
					} else {
						var stop context.CancelFunc
						ctx, stop = context.WithTimeout(ctx, time.Second)
						defer stop()
					}
				}
				done := make(chan error, 1)
				go func() {
					if transfer == "rsync" {
						done <- rsync(ctx, target, dir, "/work", []string{"ignored-by-manifest"}, io.Discard, io.Discard, opts)
					} else {
						done <- copyLocalFileToTarget(ctx, target, filepath.Join(dir, "source"), "/work/file")
					}
				}()
				pid := 0
				if mode == "cancel" || mode == "timeout" {
					pid = waitForPIDFile(t, filepath.Join(dir, "child-pid"))
					configPath, err := os.ReadFile(filepath.Join(dir, "config-path"))
					if err != nil {
						t.Fatal(err)
					}
					info, err := os.Stat(string(configPath))
					if err != nil || info.Mode().Perm() != 0o600 {
						t.Fatalf("private config must exist while transfer runs: %v", err)
					}
					parent, err := os.Stat(filepath.Dir(string(configPath)))
					if err != nil || parent.Mode().Perm() != 0o700 {
						t.Fatalf("private directory permissions: %v", err)
					}
					if mode == "cancel" {
						cancel()
					}
				}
				var runErr error
				select {
				case runErr = <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("transfer did not finish")
				}
				if (runErr == nil) != (mode == "success") {
					t.Fatalf("unexpected transfer outcome: %v", runErr)
				}
				if transfer == "rsync" && mode == "timeout" {
					var exitErr ExitError
					if !errors.As(runErr, &exitErr) || exitErr.Code != 6 {
						t.Fatalf("sync deadline lost exit code 6: %v", runErr)
					}
				}
				if pid != 0 {
					assertDescendantReaped(t, transfer, pid)
				}
				args, err := os.ReadFile(filepath.Join(dir, "argv"))
				if err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{target.User, target.Host, target.Key, target.CertificateFile} {
					if strings.Contains(string(args), secret) {
						t.Fatal("resolved SSH identity appeared in transfer argv")
					}
				}
				for _, option := range []string{"ConnectTimeout=10", "ConnectionAttempts=3"} {
					if !strings.Contains(string(args), option) {
						t.Fatalf("transfer lost %s", option)
					}
				}
				env, err := os.ReadFile(filepath.Join(dir, "environment"))
				if err != nil || string(env) != "\ntarget-sentinel\n" {
					t.Fatalf("transfer did not apply environment policy: %v", err)
				}
				config, err := os.ReadFile(filepath.Join(dir, "config"))
				if err != nil {
					t.Fatal(err)
				}
				for _, value := range []string{target.User, target.Host, target.Key, target.CertificateFile} {
					if !strings.Contains(string(config), value) {
						t.Fatal("private config lost resolved SSH identity")
					}
				}
				if transfer == "rsync" && mode == "success" {
					input, err := os.ReadFile(filepath.Join(dir, "stdin"))
					if err != nil || !bytes.Equal(input, opts.FilesFrom) {
						t.Fatalf("sync lost authoritative NUL manifest: %v", err)
					}
					if strings.Contains(string(args), "--exclude") || !strings.Contains(string(args), "--checksum") {
						t.Fatal("sync manifest/checksum option semantics changed")
					}
				}
				configPath, err := os.ReadFile(filepath.Join(dir, "config-path"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Dir(string(configPath))); !os.IsNotExist(err) {
					t.Fatalf("private transfer directory survived: %v", err)
				}
			})
		}
	}
}
