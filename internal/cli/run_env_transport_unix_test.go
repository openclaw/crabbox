//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSSHCommandEnvRealOpenSSH(t *testing.T) {
	client, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	version, err := exec.Command(client, "-V").CombinedOutput()
	if err != nil || !bytes.Contains(version, []byte("OpenSSH")) {
		t.Fatalf("real OpenSSH required: %v", err)
	}
	isolateTestUserDirs(t)
	root := t.TempDir()
	t.Logf("remote shell uid=%d", os.Geteuid())
	const user = "synthetic-env-session"
	server := newForwardSSHServer(t, user)
	target := SSHTarget{User: user, Host: "127.0.0.1", Port: strconv.Itoa(server.port()),
		SSHHostKey: server.hostKey, KnownHostsFile: filepath.Join(root, "known_hosts"),
		AuthSecret: true, NoControlMaster: true, TargetOS: targetLinux}
	if err := os.WriteFile(target.KnownHostsFile, []byte("[127.0.0.1]:"+target.Port+" "+server.hostKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := ".crabbox/env-fixture/values.sh"
	upload := uploadSSHCommandEnvCommand(target, root, path)
	command := remoteCommandWithEnvFiles(root, nil, []string{path}, []string{"/bin/sh", "-c", `printf '%s\n' "$TEST_VALUE"; cat`})
	cleanup := removeSSHCommandEnvCommand(target, root, path)
	allowed := map[string]string{"exit 0": "exit 0", upload: upload, command: command, cleanup: cleanup}
	server.mu.Lock()
	server.sessionHandler = func(incoming ssh.NewChannel, _ string) {
		ch, requests, acceptErr := incoming.Accept()
		if acceptErr != nil {
			return
		}
		defer ch.Close()
		for request := range requests {
			var payload struct{ Command string }
			if request.Type != "exec" || ssh.Unmarshal(request.Payload, &payload) != nil {
				_ = request.Reply(false, nil)
				continue
			}
			fixed, ok := allowed[payload.Command]
			if !ok {
				t.Error("SSH received a command outside the fixture allowlist")
				_ = request.Reply(false, nil)
				return
			}
			_ = request.Reply(true, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "umask 022; "+fixed)
			cmd.Env = []string{"HOME=" + root, "TMPDIR=" + root, "PATH=/usr/bin:/bin"}
			cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
			cmd.WaitDelay = time.Second
			runErr := cmd.Run()
			cancel()
			code := uint32(0)
			if runErr != nil {
				code = 1
				var exitErr *exec.ExitError
				if errors.As(runErr, &exitErr) && exitErr.ExitCode() >= 0 {
					code = uint32(exitErr.ExitCode())
				}
			}
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
			return
		}
	}
	server.mu.Unlock()
	close(server.release)
	const value = "real-ssh-env-canary '\"\nnext line"
	var stdout, stderr bytes.Buffer
	if err := runSSHInput(t.Context(), target, upload, strings.NewReader(formatShellEnvFile(map[string]string{"TEST_VALUE": value})), &stdout, &stderr); err != nil {
		t.Fatalf("upload failed: %v; %s", err, stderr.String())
	}
	for name, want := range map[string]os.FileMode{path: 0o600, shellDir(path): 0o700} {
		if info, err := os.Stat(filepath.Join(root, name)); err != nil || info.Mode().Perm() != want {
			t.Fatalf("private upload permission mismatch: %v", err)
		}
	}
	stdout.Reset()
	if err := runSSHInput(t.Context(), target, command, strings.NewReader("workload stdin"), &stdout, &stderr); err != nil || stdout.String() != value+"\nworkload stdin" {
		t.Fatalf("SSH environment/stdin round trip failed: %v", err)
	}
	if err := runSSHQuiet(t.Context(), target, cleanup); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, shellDir(path))); !os.IsNotExist(err) {
		t.Fatalf("private directory survived cleanup: %v", err)
	}
}
