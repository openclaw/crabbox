package applemachine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestRunFailureDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name, stdout, stderr string
		code                 int
	}{
		{"small", "PAYLOAD\n", "", 4},
		{"empty", "", "", 7},
		{"large stdout", strings.Repeat("X", 4<<20), "", 1},
		{"large stderr", "", strings.Repeat("S", 4<<20), 23},
		{"success", "PAYLOAD\n", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, fixture, repo := transportBackend(t)
			var stdout, stderr bytes.Buffer
			b.rt.Stdout, b.rt.Stderr = &stdout, &stderr
			var cause error
			if tc.code != 0 {
				cause = nativeExitError(t, tc.code)
			}
			fixture.before = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				if len(req.Args) < 5 || req.Args[1] != "run" || req.Args[4] != "--cwd" {
					return core.LocalCommandResult{}, nil, false
				}
				result := core.LocalCommandResult{ExitCode: tc.code, Stdout: tc.stdout, Stderr: tc.stderr}
				emitMachineOutput(req, result)
				return result, cause, true
			}
			result, err := b.Run(t.Context(), core.RunRequest{Repo: core.Repo{Root: repo}, Command: []string{"false"}})
			if result.ExitCode != tc.code || core.ExitCodeForError(err, 0) != tc.code || !errors.Is(err, cause) {
				t.Fatalf("exit=%d diagnostic code=%d; want %d", result.ExitCode, core.ExitCodeForError(err, 0), tc.code)
			}
			if stdout.String() != tc.stdout || stderr.String() != tc.stderr {
				t.Fatalf("stream lengths=%d/%d; want %d/%d", stdout.Len(), stderr.Len(), len(tc.stdout), len(tc.stderr))
			}
			if tc.code != 0 {
				want := core.Exit(tc.code, "apple-machine command exited %d", tc.code).Error()
				if err.Error() != want || result.ErrorKind != core.RunErrorCommandExit {
					t.Fatalf("diagnostic bytes=%d kind=%s; want %q", len(err.Error()), result.ErrorKind, want)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(fixture.machines) != 0 {
				t.Fatal("one-shot machine was not removed")
			}
		})
	}
}

func TestCommandDiagnosticCaptureBounded(t *testing.T) {
	b := testBackend(&recordingRunner{})
	// Exercise the production command runner: its capture limit cancels on
	// overflow, whereas diagnostic truncation must let the child finish.
	b.rt.Exec = core.RuntimeForProviderOperation(io.Discard).Exec
	b.cfg.AppleContainer.CLIPath = "/bin/sh"
	var stdout, stderr bytes.Buffer
	b.rt.Stdout, b.rt.Stderr = &stdout, &stderr
	result, err := b.command(context.Background(), []string{"-c", "head -c 4194304 /dev/zero; head -c 4194304 /dev/zero >&2; exit 23"}, "")
	if !core.IsPlainLocalCommandExit(result, err) || result.ExitCode != 23 {
		t.Fatalf("command was interrupted: code=%d err=%v", result.ExitCode, err)
	}
	if stdout.Len() != 4<<20 || stderr.Len() != 4<<20 {
		t.Fatalf("stream lengths=%d/%d", stdout.Len(), stderr.Len())
	}
	if len(result.Stdout) > 17<<10 || len(result.Stderr) > 17<<10 {
		t.Fatalf("unbounded native diagnostic capture: %d/%d", len(result.Stdout), len(result.Stderr))
	}
}

func TestRunNativeFailureDiagnosticBounded(t *testing.T) {
	b, _, fixture, repo := transportBackend(t)
	const nativeMessage = "native connection lost\n"
	payload := nativeMessage + strings.Repeat("N", 4<<20)
	cause := errors.New("private native cause")
	fixture.before = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if len(req.Args) < 5 || req.Args[1] != "run" || req.Args[4] != "--cwd" {
			return core.LocalCommandResult{}, nil, false
		}
		result := core.LocalCommandResult{ExitCode: 23, Stderr: payload}
		emitMachineOutput(req, result)
		return result, cause, true
	}
	result, err := b.Run(t.Context(), core.RunRequest{Repo: core.Repo{Root: repo}, Command: []string{"true"}})
	if result.ExitCode != 1 || result.ErrorKind != core.RunErrorProvider || !errors.Is(err, cause) {
		t.Fatalf("native outcome=%+v", result)
	}
	if !strings.Contains(err.Error(), nativeMessage) || len(err.Error()) > 17<<10 || strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("native diagnostic bytes=%d", len(err.Error()))
	}
}
