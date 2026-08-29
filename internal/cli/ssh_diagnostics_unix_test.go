//go:build !windows

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSSHDiagnosticsCancellationWithBlockedFileOutput(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	fd := int(writer.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	var block [4096]byte
	for {
		if _, err := unix.Write(fd, block[:]); errors.Is(err, unix.EAGAIN) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile :; do printf blocked; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, script)
	cmd.WaitDelay = 100 * time.Millisecond
	finished := make(chan error, 1)
	go func() {
		_, err := runSSHCommandWithLocalDiagnostics(cmd, writer, nil)
		finished <- err
	}()
	select {
	case err := <-finished:
		if err == nil || ctx.Err() == nil {
			t.Fatalf("blocked output should be canceled: err=%v context=%v", err, ctx.Err())
		}
	case <-time.After(2 * time.Second):
		_ = writer.Close()
		<-finished
		t.Fatal("diagnostic capture made native file output cancellation block")
	}
}

func TestSSHDiagnosticsDoNotWaitForPersistentWriter(t *testing.T) {
	var pipe [2]int
	if err := unix.Pipe(pipe[:]); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pipe[0])
	if err := unix.SetNonblock(pipe[0], true); err != nil {
		_ = unix.Close(pipe[1])
		t.Fatal(err)
	}
	writer, err := unix.Dup(pipe[1])
	if err != nil {
		_ = unix.Close(pipe[1])
		t.Fatal(err)
	}
	defer unix.Close(writer)
	done, wake, err := os.Pipe()
	if err != nil {
		_ = unix.Close(pipe[1])
		t.Fatal(err)
	}
	defer done.Close()
	defer wake.Close()
	log := "mm_send_fd: sendmsg(0): Message too long\n" + sshMuxDescriptorFailure + "\n"
	if _, err := unix.Write(writer, []byte(log)); err != nil {
		_ = unix.Close(pipe[1])
		t.Fatal(err)
	}
	_ = wake.Close()
	capture := sshDiagnosticCapture{output: boundedSSHOutput{limit: sshDiagnosticOutputLimit}}
	finished := make(chan bool, 1)
	go func() {
		complete, err := drainSSHDiagnostics(pipe[0], int(done.Fd()), pipe[1], &capture)
		finished <- !complete && err == nil
	}()
	select {
	case ok := <-finished:
		if !ok || capture.output.String() != log {
			t.Fatalf("capture should drain queued bytes but reject incomplete log: ok=%t output=%q", ok, capture.output.String())
		}
	case <-time.After(time.Second):
		t.Fatal("diagnostic capture waited for a persistent writer")
	}
}

func TestSSHDiagnosticsDrainQueuedLogAfterForegroundExit(t *testing.T) {
	var pipe [2]int
	if err := unix.Pipe(pipe[:]); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pipe[0])
	if err := unix.SetNonblock(pipe[0], true); err != nil {
		_ = unix.Close(pipe[1])
		t.Fatal(err)
	}
	done, wake, err := os.Pipe()
	if err != nil {
		_ = unix.Close(pipe[1])
		t.Fatal(err)
	}
	defer done.Close()
	defer wake.Close()
	log := "mm_send_fd: sendmsg(0): Message too long\n" + sshMuxDescriptorFailure + "\n"
	if _, err := unix.Write(pipe[1], []byte(log)); err != nil {
		_ = unix.Close(pipe[1])
		t.Fatal(err)
	}
	_ = wake.Close()
	capture := sshDiagnosticCapture{output: boundedSSHOutput{limit: sshDiagnosticOutputLimit}}
	complete, err := drainSSHDiagnostics(pipe[0], int(done.Fd()), pipe[1], &capture)
	if err != nil || !complete || !capture.detector.failed() {
		t.Fatalf("queued complete diagnostic not recognized: complete=%t err=%v matched=%t", complete, err, capture.detector.failed())
	}
}
