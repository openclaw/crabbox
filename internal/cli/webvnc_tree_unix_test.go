//go:build darwin || linux

package cli

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWebVNCDaemonStopRefusesOrphanedProcessGroupWithoutSupervisorIdentity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.pid")
	heartbeatPath := filepath.Join(dir, "heartbeat")
	nonce := "0123456789abcdef0123456789abcdef"
	script := `sh -c 'while :; do printf x >>"$1"; sleep 0.05; done' crabbox-webvnc-child "$2" & echo $! >"$1"; wait`
	cmd := exec.Command("sh", "-c", script, "crabbox-webvnc-test-"+nonce, childPath, heartbeatPath)
	configureDaemonCommand(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = terminateWebVNCDaemonProcessTree(cmd.Process.Pid)
		_, _ = cmd.Process.Wait()
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(childPath); err == nil {
			if info, heartbeatErr := os.Stat(heartbeatPath); heartbeatErr == nil && info.Size() > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("orphan fixture did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	started, err := webVNCDaemonProcessStartIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	_, pidPath, err := webVNCDaemonPaths("workspace-orphan")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeWebVNCDaemonIdentity(pidPath, webVNCDaemonIdentity{
		Version: webVNCDaemonIdentityVersion, WorkspaceID: "workspace-orphan", PID: cmd.Process.Pid,
		ProcessStarted: started, BootID: currentProcessBootIdentityForTest(t), Nonce: nonce,
	}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()
	if !webVNCDaemonProcessGroupAlive(cmd.Process.Pid) {
		t.Fatal("fixture descendant did not survive its supervisor")
	}
	var stdout bytes.Buffer
	stopped, err := (App{Stdout: &stdout, Stderr: io.Discard}).stopWebVNCDaemonIfRunning("workspace-orphan")
	if err == nil || stopped || !strings.Contains(err.Error(), "without its recorded supervisor identity") {
		t.Fatalf("stopped=%t output=%q err=%v", stopped, stdout.String(), err)
	}
	if !webVNCDaemonProcessGroupAlive(cmd.Process.Pid) {
		t.Fatal("unverifiable WebVNC process group was signaled")
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("durable identity was removed after refused termination: %v", err)
	}
}
