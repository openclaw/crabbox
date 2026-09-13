package shared

import (
	"errors"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestLocalCommandError(t *testing.T) {
	cause := errors.New("process failed")
	for _, tt := range []struct {
		name   string
		result core.LocalCommandResult
		cause  error
		code   int
		text   string
	}{
		{"stderr takes precedence", core.LocalCommandResult{ExitCode: 5, Stderr: " detail\n", Stdout: "ignored"}, cause, 5, "tool failed: process failed: detail"},
		{"stdout fallback", core.LocalCommandResult{ExitCode: 2, Stderr: "\t\n", Stdout: " output\n"}, cause, 2, "tool failed: process failed: output"},
		{"cause only", core.LocalCommandResult{ExitCode: 7}, cause, 7, "tool failed: process failed"},
		{"zero becomes failure", core.LocalCommandResult{}, cause, 1, "tool failed: process failed"},
		{"signed exit preserved", core.LocalCommandResult{ExitCode: -1}, cause, -1, "tool failed: process failed"},
		{"nil cause with output", core.LocalCommandResult{ExitCode: 3, Stderr: "detail"}, nil, 3, "tool failed: <nil>: detail"},
		{"nil cause without output", core.LocalCommandResult{}, nil, 1, "tool failed: <nil>"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := LocalCommandError("tool", tt.result, tt.cause)
			var exitErr core.ExitError
			if !core.AsExitError(err, &exitErr) || exitErr.Code != tt.code || err.Error() != tt.text {
				t.Fatalf("got %T (%d, %q), want ExitError (%d, %q)", err, exitErr.Code, err.Error(), tt.code, tt.text)
			}
		})
	}
}
