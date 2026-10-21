package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestDetachedJobRealSSH(t *testing.T) {
	clearConfigEnv(t)
	dirs := isolateTestUserDirs(t)
	guest := t.TempDir()
	server := newForwardSSHServer(t, "crabbox")
	server.mu.Lock()
	server.sessionHandler = func(incoming ssh.NewChannel, _ string) {
		ch, requests, err := incoming.Accept()
		if err != nil {
			return
		}
		defer ch.Close()
		for request := range requests {
			var payload struct{ Command string }
			if request.Type != "exec" || ssh.Unmarshal(request.Payload, &payload) != nil {
				_ = request.Reply(false, nil)
				continue
			}
			_ = request.Reply(true, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", payload.Command)
			cmd.Env = []string{"HOME=" + guest, "PATH=/usr/bin:/bin"}
			cmd.Dir = guest
			cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
			cmd.WaitDelay = time.Second
			err = cmd.Run()
			cancel()
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(exitCode(err))}))
			return
		}
	}
	server.mu.Unlock()
	close(server.release)
	sshDir := filepath.Join(dirs.Home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("Host job-fixture\n HostName 127.0.0.1\n Port %d\n User crabbox\n IdentitiesOnly yes\n ProxyCommand nc 127.0.0.1 %%p\n", server.port())
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	knownHost := fmt.Sprintf("[127.0.0.1]:%d %s\n[job-fixture]:%d %s\n", server.port(), server.hostKey, server.port(), server.hostKey)
	if err := os.WriteFile(filepath.Join(sshDir, "known_hosts"), []byte(knownHost), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := runEnvProfileTestProvider{}
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dirs.Home, "missing.yaml"))
	t.Setenv("CRABBOX_PROVIDER", provider.Spec().Name)
	t.Setenv("CRABBOX_WORK_ROOT", filepath.Join(guest, "work"))
	lease := LeaseTarget{
		LeaseID: "cbx_job_fixture",
		Server:  Server{CloudID: "job-fixture", Provider: provider.Spec().Name, Labels: map[string]string{"lease": "cbx_job_fixture", "provider": provider.Spec().Name, "slug": "job-fixture", "state": "ready"}},
		SSH:     SSHTarget{Host: "job-fixture", User: "crabbox", Port: strconv.Itoa(server.port()), TargetOS: targetLinux, SSHConfigProxy: true, SSHConfigFile: filepath.Join(sshDir, "config"), SSHHostKey: server.hostKey, KnownHostsFile: filepath.Join(sshDir, "known_hosts"), NoControlMaster: true, FallbackPorts: []string{}},
	}
	runEnvProfileTestAcquireLease = func(AcquireRequest) (LeaseTarget, error) { return lease, nil }
	t.Cleanup(func() { runEnvProfileTestAcquireLease = nil })
	repo, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(guest, "work", lease.LeaseID, repo.Name)
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, operation string, args ...string) (string, error) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		argv := append([]string{"job", operation, "--id", lease.LeaseID}, args...)
		err := (App{Stdout: &stdout, Stderr: &stderr}).Run(ctx, argv)
		if err != nil {
			t.Logf("%s: %s", operation, stderr.String())
		}
		return stdout.String(), err
	}
	out, err := run(t.Context(), "submit", "--job-id", "trial", "--", "sh", "-c", "printf 'alpha\\nbeta\\n'; sleep 2; printf result > result.txt; exit 37")
	if err != nil || !strings.Contains(out, `"submitted"`) {
		t.Fatalf("submit=%q err=%v", out, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		out, err = run(t.Context(), "status", "--job-id", "trial")
		var status detachedJobStatus
		if err != nil || json.Unmarshal([]byte(out), &status) != nil {
			t.Fatalf("status=%q err=%v", out, err)
		}
		if status.State == "exited" {
			if status.ExitCode == nil || *status.ExitCode != 37 || status.PID <= 0 {
				t.Fatalf("wrong workload result: %+v", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job remained %s", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	out, err = run(t.Context(), "logs", "--job-id", "trial", "--lines", "1")
	if err != nil || out != "beta\n" {
		t.Fatalf("logs=%q err=%v", out, err)
	}
	if out, err := run(t.Context(), "submit", "--job-id", "trial", "--", "touch", "replayed"); err == nil {
		t.Fatalf("duplicate submission accepted: %s", out)
	}
	if _, err := os.Stat(filepath.Join(workdir, "replayed")); !os.IsNotExist(err) {
		t.Fatal("duplicate executed")
	}
	local := filepath.Join(t.TempDir(), "payload.txt")
	if err := os.WriteFile(local, []byte("transfer-payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t.Context(), "push", local, "input.txt"); err != nil {
		t.Fatalf("push=%q err=%v", out, err)
	}
	if data, err := os.ReadFile(filepath.Join(workdir, "input.txt")); err != nil || string(data) != "transfer-payload\n" {
		t.Fatalf("uploaded bytes=%q err=%v", data, err)
	}
	download := filepath.Join(t.TempDir(), "result.txt")
	if out, err := run(t.Context(), "pull", download, "result.txt"); err != nil {
		t.Fatalf("pull=%q err=%v", out, err)
	}
	if data, err := os.ReadFile(download); err != nil || string(data) != "result" {
		t.Fatalf("downloaded bytes=%q err=%v", data, err)
	}
	if err := os.Symlink(guest, filepath.Join(workdir, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t.Context(), "push", local, "escape/outside.txt"); err == nil {
		t.Fatal("symlink escape accepted")
	}
	if _, err := os.Stat(filepath.Join(guest, "outside.txt")); !os.IsNotExist(err) {
		t.Fatal("symlink escape wrote outside workspace")
	}
	server.listener.Close()
	_, err = run(t.Context(), "status", "--job-id", "trial")
	if ExitCodeForError(err, 0) != 7 {
		t.Fatalf("transport failure became workload result: %v", err)
	}
}
