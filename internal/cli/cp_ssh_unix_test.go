//go:build !windows

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
	"syscall"
	"testing"
	"time"
)

// assertDescendantReaped polls until the process is gone (ESRCH). Descendant
// teardown after context cancellation is asynchronous at the OS level and can
// still be in flight when the cancelled command returns, so a single immediate
// Kill(pid, 0) check is racy under load (observed as CI flakes). Poll within a
// bounded deadline instead.
func assertDescendantReaped(t *testing.T, label string, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s descendant %d survived cancellation: %v", label, pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestResolvedSSHRemoteSecludedArgsProbeHonorsCancellation(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	pidPath := filepath.Join(dir, "child-pid")
	script := "#!/bin/sh\nsleep 30 &\nchild=$!\nprintf '%s' \"$child\" > \"$CRABBOX_TEST_SSH_CHILD_PID\"\nwait \"$child\"\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_TEST_SSH_CHILD_PID", pidPath)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- probeResolvedSSHRemoteSecludedArgs(ctx, &sshTransportSession{configPath: "/private/config"}, SSHTarget{}, "")
	}()
	childPID := waitForPIDFile(t, pidPath)
	cancel()
	err := <-result
	if err == nil {
		t.Fatal("expected probe error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("probe error=%v", err)
	}
	assertDescendantReaped(t, "probe", childPID)
}

func TestOwnedSSHTransportCommandReapsDescendants(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	pidPath := filepath.Join(dir, "owned-child-pid")
	script := "#!/bin/sh\nsleep 30 &\nchild=$!\nprintf '%s' \"$child\" > \"$CRABBOX_TEST_OWNED_SSH_CHILD_PID\"\nwait \"$child\"\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_TEST_OWNED_SSH_CHILD_PID", pidPath)
	if resolved, err := exec.LookPath("ssh"); err != nil || resolved != sshPath {
		t.Fatalf("fake ssh resolution=%q err=%v", resolved, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- runOwnedSSHTransportCommand(ctx, SSHTarget{}, []string{"-G", "example.test"}, &bytes.Buffer{}, &bytes.Buffer{})
	}()
	childPID := waitForPIDFile(t, pidPath)
	cancel()
	err := <-result
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("owned command err=%v", err)
	}
	assertDescendantReaped(t, "owned SSH", childPID)
}

func TestRsyncRemoteShellRoundTripsApostrophePath(t *testing.T) {
	rsyncPath, err := exec.LookPath("rsync")
	if err != nil {
		t.Skip("rsync is required")
	}
	dir := filepath.Join(t.TempDir(), "O'Brien")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	capturePath := filepath.Join(dir, "args")
	sshPath := filepath.Join(dir, "ssh")
	if err := os.WriteFile(sshPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CRABBOX_TEST_SSH_ARGS\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(dir, "source")
	if err := os.WriteFile(sourcePath, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_TEST_SSH_ARGS", capturePath)
	session := &sshTransportSession{configPath: filepath.Join(dir, "ssh config")}
	cmd := exec.Command(rsyncPath, "-e", session.rsyncRemoteShell(), "--", sourcePath, sshTransportHostAlias+":/tmp/destination")
	cmd.Env = os.Environ()
	_ = cmd.Run()
	data, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	wantPrefix := []string{"-F", session.configPath, sshTransportHostAlias}
	if len(args) < len(wantPrefix) {
		t.Fatalf("ssh args=%#v", args)
	}
	for index, want := range wantPrefix {
		if args[index] != want {
			t.Fatalf("ssh arg %d=%q, want %q; all=%#v", index, args[index], want, args)
		}
	}
}

func TestCopyOverResolvedSSHCancellationReapsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "child-pid")
	rsyncPath := filepath.Join(dir, "rsync")
	script := `#!/bin/sh
set -eu
case " $* " in
  *" --version "*) printf 'rsync  version 3.4.4  protocol version 32\n'; exit 0 ;;
esac
sleep 30 &
child=$!
printf '%s' "$child" > "$CRABBOX_TEST_RSYNC_CHILD_PID"
wait "$child"
`
	if err := os.WriteFile(rsyncPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRABBOX_TEST_RSYNC_CHILD_PID", pidPath)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- copyOverResolvedSSH(ctx, SSHTarget{User: "alice", Host: "example.test", Port: "22"}, "./input", "SANDBOX:/tmp/input", false, &bytes.Buffer{}, &bytes.Buffer{})
	}()
	deadline := time.Now().Add(5 * time.Second)
	var childPID int
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("rsync descendant did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled copy err=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled copy did not return")
	}
	assertDescendantReaped(t, "rsync", childPID)
}
