//go:build linux

package cli

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStopLocalEgressClientSessionPreservesSuccessor(t *testing.T) {
	isolateRunTestUserDirs(t, t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	type child struct {
		input  io.WriteCloser
		output *bufio.Reader
		wait   <-chan error
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	otherExe := filepath.Join(t.TempDir(), "other-executable")
	if err := os.Link(exe, otherExe); err != nil {
		t.Fatal(err)
	}
	start := func(session, phase, executable string) child {
		t.Helper()
		cmd := exec.CommandContext(ctx, executable)
		cmd.Args = []string{egressRemoteBinary, "egress", "client", phase, "--id", "crabbox-session-cleanup-test", "--session", session}
		cmd.Env = append(os.Environ(), "CRABBOX_TEST_EGRESS_SESSION_CHILD=1")
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		wait := make(chan error, 1)
		go func() { wait <- cmd.Wait() }()
		t.Cleanup(func() { _ = input.Close(); _ = cmd.Process.Kill() })
		reader := bufio.NewReader(output)
		if line, err := reader.ReadString('\n'); err != nil || line != "ready\n" {
			t.Fatalf("child readiness = %q, %v", line, err)
		}
		return child{input, reader, wait}
	}
	old := start("old", egressTicketChildArg, exe)
	bootstrap := start("old", egressTicketStdinArg, exe)
	successor := start("old-successor", egressTicketChildArg, exe)
	forged := start("old", egressTicketChildArg, otherExe)
	if err := stopLocalEgressClientSession(ctx, "crabbox-session-cleanup-test", "old"); err != nil {
		t.Fatal(err)
	}
	for _, matched := range []child{old, bootstrap} {
		select {
		case <-matched.wait:
		case <-ctx.Done():
			t.Fatal("matching session did not exit")
		}
	}
	for _, preserved := range []child{successor, forged} {
		if _, err := io.WriteString(preserved.input, "still-alive\n"); err != nil {
			t.Fatal(err)
		}
		if line, err := preserved.output.ReadString('\n'); err != nil || line != "still-alive\n" {
			t.Fatalf("preserved process = %q, %v", line, err)
		}
	}
	if err := stopLocalEgressClientSession(ctx, "crabbox-session-cleanup-test", "old"); err != nil {
		t.Fatalf("repeated cleanup must be harmless: %v", err)
	}
	app := App{Stdin: strings.NewReader(receiverTestTicket), Stdout: io.Discard, Stderr: io.Discard}
	if err := app.egressClient(ctx, []string{egressTicketStdinArg, "--id", "crabbox-session-cleanup-test", "--session", "old"}); err == nil || !strings.Contains(err.Error(), "session was stopped") {
		t.Fatalf("late bootstrap escaped session cleanup: %v", err)
	}
	if err := stopLocalEgressClientSession(ctx, "crabbox-session-cleanup-test", "old-successor"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-successor.wait:
	case <-ctx.Done():
		t.Fatal("successor cleanup did not exit")
	}
	_ = forged.input.Close()
	select {
	case <-forged.wait:
	case <-ctx.Done():
		t.Fatal("forged-argv fixture did not exit")
	}
}
