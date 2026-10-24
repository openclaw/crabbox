package shared

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestDelegatedSandboxArtifactsPreservePrimaryOutcome(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		code                         int
		transport, artifact, cleanup error
		wantCode                     int
		wantKind                     core.RunErrorKind
		wantEvents                   []string
	}{
		{name: "success", wantEvents: []string{"command", "artifacts", "cleanup"}},
		{name: "failed command evidence", code: 23, wantCode: 23, wantKind: core.RunErrorCommandExit, wantEvents: []string{"command", "artifacts", "cleanup"}},
		{name: "artifact failure", artifact: core.Exit(7, "missing required artifact"), wantCode: 7, wantKind: core.RunErrorProvider, wantEvents: []string{"command", "artifacts", "cleanup"}},
		{name: "command artifact cleanup failure", code: 23, artifact: core.Exit(7, "artifact failed"), cleanup: errors.New("cleanup failed"), wantCode: 23, wantKind: core.RunErrorCommandExit, wantEvents: []string{"command", "artifacts", "cleanup"}},
		{name: "transport withholds evidence", transport: io.ErrUnexpectedEOF, wantCode: 1, wantKind: core.RunErrorProvider, wantEvents: []string{"command", "cleanup"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			result, err := RunDelegatedSandbox(context.Background(), core.RunRequest{NoSync: true}, DelegatedSandboxLifecycle{
				Provider: "fixture", Runtime: core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
				Acquire: func(context.Context) (DelegatedSandbox, error) { return DelegatedSandbox{LeaseID: "lease"}, nil },
				Workspace: func() SandboxWorkspace {
					return WorkspaceOperations{EnsureFunc: func(context.Context) error { return nil }}
				},
				Command: func(context.Context) (DelegatedSandboxCommand, error) {
					return DelegatedSandboxCommand{Run: func(context.Context, io.Writer, io.Writer) (int, error) {
						events = append(events, "command")
						return tc.code, tc.transport
					}}, nil
				},
				Artifacts: func(context.Context) ([]core.RunArtifact, error) {
					events = append(events, "artifacts")
					return []core.RunArtifact{{Kind: "fixture", Path: "proof"}}, tc.artifact
				},
				Cleanup: func(context.Context) error { events = append(events, "cleanup"); return tc.cleanup },
			})
			if result.ExitCode != tc.wantCode || result.ErrorKind != tc.wantKind || !reflect.DeepEqual(events, tc.wantEvents) {
				t.Fatalf("result=%+v events=%v error=%v", result, events, err)
			}
			if tc.transport != nil && len(result.Artifacts) != 0 {
				t.Fatal("transport failure published artifacts")
			}
			if tc.artifact != nil && (err == nil || !strings.Contains(err.Error(), tc.artifact.Error())) {
				t.Fatalf("artifact diagnostic lost: %v", err)
			}
			if tc.cleanup != nil && (err == nil || !strings.Contains(err.Error(), tc.cleanup.Error())) {
				t.Fatalf("cleanup diagnostic lost: %v", err)
			}
		})
	}
}
