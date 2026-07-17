//go:build !windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunPondMeshForwardsUnexpectedSuccessKillsDescendants(t *testing.T) {
	binDir := t.TempDir()
	stateDir := t.TempDir()
	descendantFile := filepath.Join(stateDir, "descendant")
	fakeSSH := filepath.Join(binDir, "ssh")
	script := "#!/bin/sh\nsleep 600 &\necho $! > \"$CBX_POND_DESCENDANT\"\nexit 0\n"
	if err := os.WriteFile(fakeSSH, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CBX_POND_DESCENDANT", descendantFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	members := []pondMember{{
		Name: "peer-a", Lease: "lease-a",
		SSH: SSHTarget{User: "crab", Host: "127.0.0.1", Port: "22", DisableHostKeyChecking: true},
	}}
	summary := pondMeshSummary{Forwards: []pondMeshForward{{
		Peer: "peer-a", RemotePort: 8080, LocalPort: 18080, LeaseID: "lease-a",
	}}}
	err := runPondMeshForwards(context.Background(), pondConnectOptions{Stdout: io.Discard, Stderr: io.Discard}, members, summary)
	if err == nil || !strings.Contains(err.Error(), "ssh forwards for peer-a:8080 exited unexpectedly") {
		t.Fatalf("unexpected clean forward exit = %v", err)
	}
	descendantPID := readPondMeshSignalPID(t, descendantFile)
	defer func() { _ = syscall.Kill(descendantPID, syscall.SIGKILL) }()
	waitForPondMeshSignalProcessExit(t, descendantPID)
}

func TestRunPondMeshForwardsTerminalInterruptIsClean(t *testing.T) {
	if os.Getenv("CBX_POND_SIGNAL_HELPER") == "1" {
		runPondMeshTerminalInterruptHelper(t)
		return
	}

	for _, tc := range []struct {
		name   string
		signal syscall.Signal
	}{
		{name: "interrupt", signal: syscall.SIGINT},
		{name: "hangup", signal: syscall.SIGHUP},
		{name: "quit", signal: syscall.SIGQUIT},
		{name: "suspend", signal: syscall.SIGTSTP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			stateDir := t.TempDir()
			readyFile := filepath.Join(stateDir, "ready")
			childFile := filepath.Join(stateDir, "child")
			descendantFile := filepath.Join(stateDir, "descendant")
			fakeSSH := filepath.Join(binDir, "ssh")
			script := "#!/bin/sh\necho $$ > \"$CBX_POND_SIGNAL_CHILD\"\nsleep 600 &\necho $! > \"$CBX_POND_SIGNAL_DESCENDANT\"\necho ready > \"$CBX_POND_SIGNAL_READY\"\ntrap 'exit 255' HUP INT QUIT TERM\nwhile :; do sleep 0.05; done\n"
			if err := os.WriteFile(fakeSSH, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(os.Args[0], "-test.run=^TestRunPondMeshForwardsTerminalInterruptIsClean$")
			cmd.Env = append(os.Environ(),
				"CBX_POND_SIGNAL_HELPER=1",
				"CBX_POND_SIGNAL_KIND="+tc.name,
				"CBX_POND_SIGNAL_READY="+readyFile,
				"CBX_POND_SIGNAL_CHILD="+childFile,
				"CBX_POND_SIGNAL_DESCENDANT="+descendantFile,
				"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				if data, err := os.ReadFile(childFile); err == nil {
					if pid, err := strconv.Atoi(string(bytes.TrimSpace(data))); err == nil {
						_ = syscall.Kill(-pid, syscall.SIGKILL)
					}
				}
			}()

			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(readyFile); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("forward never became ready:\n%s", output.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := syscall.Kill(-cmd.Process.Pid, tc.signal); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatalf("terminal %s reported a tunnel failure: %v\n%s", tc.name, err, output.String())
			}
			descendantPID := readPondMeshSignalPID(t, descendantFile)
			waitForPondMeshSignalProcessExit(t, descendantPID)
		})
	}
}

func readPondMeshSignalPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(bytes.TrimSpace(data)))
	if err != nil {
		t.Fatalf("parse descendant pid: %v", err)
	}
	return pid
}

func waitForPondMeshSignalProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil && !errors.Is(err, syscall.EPERM) {
			t.Fatalf("probe descendant %d: %v", pid, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("forward descendant %d survived cancellation", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runPondMeshTerminalInterruptHelper(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if os.Getenv("CBX_POND_SIGNAL_KIND") == "interrupt" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt)
		defer signal.Stop(signals)
		go func() {
			<-signals
			// A terminal delivers SIGINT to the full foreground process group.
			// Delay parent cancellation so group isolation is exercised.
			time.Sleep(200 * time.Millisecond)
			cancel()
		}()
	}

	members := []pondMember{{
		Name:  "peer-a",
		Lease: "lease-a",
		SSH:   SSHTarget{User: "crab", Host: "127.0.0.1", Port: "22", DisableHostKeyChecking: true, NoControlMaster: true},
	}}
	summary := pondMeshSummary{Forwards: []pondMeshForward{{
		Peer: "peer-a", RemotePort: 8080, LocalPort: 18080, LeaseID: "lease-a",
	}}}
	if err := runPondMeshForwards(ctx, pondConnectOptions{Stdout: io.Discard, Stderr: io.Discard}, members, summary); err != nil {
		t.Fatalf("clean terminal interrupt: %v", err)
	}
}
