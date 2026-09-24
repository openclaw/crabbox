package main

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/openclaw/crabbox/internal/cli"
)

func TestPrintErrorPreservesJoinedCauses(t *testing.T) {
	cause := errors.New("boxd gRPC API returned FailedPrecondition: billing is not enabled")
	for _, tt := range []struct {
		name string
		err  error
		want string
		code int
	}{
		{"nil", nil, "", 1},
		{"ordinary", cause, cause.Error() + "\n", 1},
		{"exit", cli.Exit(5, "retained claim"), "retained claim\n", 5},
		{"silent exit", cli.Exit(37, ""), "", 37},
		{"wrapped exit", fmt.Errorf("context: %w", cli.Exit(5, "retained claim")), "retained claim\n", 5},
		{"joined", errors.Join(cause, cli.Exit(5, "retained claim")), cause.Error() + "\nretained claim\n", 5},
		{"silent joined", errors.Join(cli.Exit(37, ""), cause), cause.Error() + "\n", 37},
		{"nested", fmt.Errorf("context: %w", errors.Join(cli.Exit(5, "retained claim"), errors.Join(cause, cli.Exit(7, "cleanup failed")))), "retained claim\n" + cause.Error() + "\ncleanup failed\n", 5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			printError(&output, tt.err)
			if output.String() != tt.want {
				t.Fatalf("output=%q want=%q", output.String(), tt.want)
			}
			if code := cli.ExitCodeForError(tt.err, 1); code != tt.code {
				t.Fatalf("code=%d want=%d", code, tt.code)
			}
		})
	}
}
