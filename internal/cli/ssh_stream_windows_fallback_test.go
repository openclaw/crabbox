//go:build windows

package cli

import (
	"errors"
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
	var limitErr *commandStreamLimitError
	if !errors.As(err, &limitErr) || !strings.Contains(streamFailureMessage(err, "fallback"), "temporary spool limit") {
		t.Fatalf("err=%v, want spool limit error", err)
	}
}

func TestAbandonCommandSuspensionDoesNotContinueAfterFailedRollback(t *testing.T) {
	resumeFailure := errors.New("resume denied")
	handles := []windows.Handle{1, 2}
	for _, tc := range []struct {
		name            string
		cause           error
		resumeErr       error
		wantUnavailable bool
	}{
		{name: "denied and rolled back", cause: windows.ERROR_ACCESS_DENIED, wantUnavailable: true},
		{name: "denied but rollback failed", cause: windows.ERROR_ACCESS_DENIED, resumeErr: resumeFailure},
		{name: "other failure", cause: windows.ERROR_NOT_ENOUGH_MEMORY},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var resumed []windows.Handle
			err := abandonCommandSuspension(handles, tc.cause, func(got []windows.Handle) error {
				resumed = got
				return tc.resumeErr
			})
			if len(resumed) != len(handles) {
				t.Fatalf("resumed %v, want %v", resumed, handles)
			}
			if got := errors.Is(err, errCommandStreamSuspensionUnavailable); got != tc.wantUnavailable {
				t.Fatalf("unavailable=%v, want %v (err=%v)", got, tc.wantUnavailable, err)
			}
			if tc.resumeErr != nil && !errors.Is(err, tc.resumeErr) {
				t.Fatalf("err=%v does not report rollback failure", err)
			}
			if !errors.Is(err, tc.cause) {
				t.Fatalf("err=%v lost cause", err)
			}
		})
	}
}
