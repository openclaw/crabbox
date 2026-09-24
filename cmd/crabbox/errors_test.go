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
		{"empty ordinary", errors.New(""), "\n", 1},
		{"wrapped ordinary", fmt.Errorf("context: %w", cause), "context: " + cause.Error() + "\n", 1},
		{"multi wrapped ordinary", fmt.Errorf("create: %w; cleanup: %w", cause, errors.New("disk full")), "create: " + cause.Error() + "; cleanup: disk full\n", 1},
		{"equal ordinary causes", errors.Join(cause, cause), cause.Error() + "\n", 1},
		{"exit", cli.Exit(5, "retained claim"), "retained claim\n", 5},
		{"silent exit", cli.Exit(37, ""), "", 37},
		{"wrapped exit", fmt.Errorf("context: %w", cli.Exit(5, "retained claim")), "retained claim\n", 5},
		{"joined", errors.Join(cause, cli.Exit(5, "retained claim")), "retained claim\n" + cause.Error() + "\n", 5},
		{"silent joined", errors.Join(cli.Exit(37, ""), cause), cause.Error() + "\n", 37},
		{"nested", fmt.Errorf("context: %w", errors.Join(cli.Exit(5, "retained claim"), errors.Join(cause, cli.Exit(7, "cleanup failed")))), "retained claim\n" + cause.Error() + "\ncleanup failed\n", 5},
		{"cause already in exit", errors.Join(cli.Exit(5, "create failed: %v", cause), cause), "create failed: " + cause.Error() + "\n", 5},
		{"cause before exit", errors.Join(cause, cli.Exit(5, "create failed: %v", cause)), "create failed: " + cause.Error() + "\n", 5},
		{"equal exit", errors.Join(cause, cli.Exit(5, "%v", cause)), cause.Error() + "\n", 5},
		{"equal printed cause", errors.Join(cli.Exit(5, "retained claim"), cause, cause), "retained claim\n" + cause.Error() + "\n", 5},
		{"contained printed cause", errors.Join(cli.Exit(5, "retained claim"), fmt.Errorf("create failed: %w", cause), cause), "retained claim\ncreate failed: " + cause.Error() + "\n", 5},
		{"contained exit", errors.Join(cli.Exit(5, "cleanup failed: disk full"), cli.Exit(7, "disk full")), "cleanup failed: disk full\n", 5},
		{"nested preformatted exit", errors.Join(cli.Exit(7, "cleanup failed: %v", cause), errors.Join(fmt.Errorf("%w", cause), cause)), "cleanup failed: " + cause.Error() + "\n", 7},
		{"nested multi wrapped ordinary", errors.Join(cli.Exit(5, "retained claim"), fmt.Errorf("create: %w; cleanup: %w", cause, errors.New("disk full")), cause), "retained claim\ncreate: " + cause.Error() + "; cleanup: disk full\n", 5},
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
