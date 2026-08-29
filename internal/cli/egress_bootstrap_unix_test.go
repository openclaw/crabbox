//go:build !windows

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEgressBootstrapWaitsForTicketEOF(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"pkill":  "#!/bin/sh\nexit 0\n",
		"helper": "#!/bin/sh\nexec " + shellQuote(exe) + " \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	remote := remoteEgressClientCommand("http://127.0.0.1:1", "cbx_0123456789ab", "egress_bootstrap_test", "127.0.0.1:0")
	remote = strings.ReplaceAll(remote, egressRemoteLog, filepath.Join(dir, "helper.log"))
	remote = strings.ReplaceAll(remote, egressRemoteBinary, filepath.Join(dir, "helper"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", remote)
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + dir, "CRABBOX_CONFIG=" + filepath.Join(dir, "missing.yaml"), "CRABBOX_TEST_EGRESS_BOOTSTRAP=" + dir}
	configureDaemonCommand(cmd)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	reaped := false
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if data, err := os.ReadFile(filepath.Join(dir, "child-started")); err == nil {
			if pid, err := strconv.Atoi(string(data)); err == nil {
				_ = terminateWebVNCDaemonProcessTree(pid)
			}
		}
		if !reaped {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("launcher not reaped")
			}
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "bootstrap-started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Go receiver did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		reaped = true
		t.Fatalf("launcher exited before delayed ticket input: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := io.WriteString(input, receiverTestTicket); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		reaped = true
		t.Fatalf("launcher exited before ticket EOF: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	_ = input.Close()
	select {
	case err := <-done:
		reaped = true
		if err != nil {
			t.Fatalf("launcher failed after full input: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("launcher did not return after EOF")
	}
	// Keep the detached child waiting while asserting the launcher has returned.
	pid := waitForPIDFile(t, filepath.Join(dir, "child-started"))
	if !egressDaemonTestAlive(pid) {
		t.Fatal("detached child did not outlive bootstrap")
	}
}

type egressFailingHandoff struct{ mode string }

func (w egressFailingHandoff) Write(p []byte) (int, error) {
	if w.mode == "write" {
		return 0, errors.New(receiverTestTicket)
	}
	if w.mode == "short" {
		return len(p) - 1, nil
	}
	return len(p), nil
}
func (w egressFailingHandoff) Close() error {
	if w.mode == "close" {
		return errors.New(receiverTestTicket)
	}
	return nil
}

func TestEgressBootstrapLaunchFailuresCleanUp(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		cmd := exec.Command(filepath.Join(t.TempDir(), receiverTestTicket))
		err := launchEgressClientProcess(cmd, receiverTestTicket)
		if err == nil || strings.Contains(err.Error(), receiverTestTicket) || cmd.Process != nil {
			t.Fatalf("unsafe start failure: %v", err)
		}
	})
	for _, mode := range []string{"write", "short", "close"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command("/bin/sleep", "30")
			configureDaemonCommand(cmd)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			pid := cmd.Process.Pid
			defer func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() }()
			err := completeEgressClientLaunch(cmd, egressFailingHandoff{mode}, receiverTestTicket)
			if err == nil || strings.Contains(err.Error(), receiverTestTicket) {
				t.Fatalf("unsafe handoff error: %v", err)
			}
			if cmd.ProcessState == nil || webVNCDaemonProcessGroupAlive(pid) {
				t.Fatal("failed handoff did not kill and reap child")
			}
		})
	}
}

func TestEgressBootstrapRejectsSSHOutputPipes(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	log, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	for _, app := range []App{{Stdout: writer, Stderr: log}, {Stdout: log, Stderr: writer}, {Stdout: io.Discard, Stderr: log}} {
		if err := app.startEgressClientProcess(nil, receiverTestTicket); err == nil || !strings.Contains(err.Error(), "regular log files") {
			t.Fatalf("unsafe output handle accepted: %v", err)
		}
	}
}
