//go:build windows

package cli

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestRunCommandWithPlatformStreamsContinuesWhenSuspensionIsUnavailable(t *testing.T) {
	previousRotateSize := commandStreamRotateSize
	commandStreamRotateSize = 8 * 1024 * 1024
	defer func() {
		commandStreamRotateSize = previousRotateSize
	}()
	previousSuspend := commandStreamSuspendProcess
	suspendCalls := 0
	commandStreamSuspendProcess = func(uint32) ([]windows.Handle, error) {
		suspendCalls++
		return nil, errCommandStreamSuspensionUnavailable
	}
	defer func() {
		commandStreamSuspendProcess = previousSuspend
	}()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCommandWithPlatformStreamsRotatesLargeOutput$")
	cmd.Env = append(os.Environ(), "CRABBOX_LARGE_FILE_STREAM_HELPER=1")
	if err := runCommandWithPlatformStreams(cmd, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if suspendCalls != 1 {
		t.Fatalf("suspend calls=%d, want 1 before rotation is disabled", suspendCalls)
	}
}

func TestRunCommandWithPlatformStreamsFailsWhenFallbackSpoolExceedsLimit(t *testing.T) {
	previousRotateSize := commandStreamRotateSize
	commandStreamRotateSize = 1024 * 1024
	defer func() {
		commandStreamRotateSize = previousRotateSize
	}()
	previousSuspend := commandStreamSuspendProcess
	commandStreamSuspendProcess = func(uint32) ([]windows.Handle, error) {
		return nil, errCommandStreamSuspensionUnavailable
	}
	defer func() {
		commandStreamSuspendProcess = previousSuspend
	}()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCommandWithPlatformStreamsRotatesLargeOutput$")
	cmd.Env = append(os.Environ(), "CRABBOX_LARGE_FILE_STREAM_HELPER=1")
	err := runCommandWithPlatformStreams(cmd, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "temporary spool limit") {
		t.Fatalf("err=%v, want spool limit error", err)
	}
}
