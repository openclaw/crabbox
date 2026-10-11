package main

import (
	"bytes"
	"encoding/json"
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

func TestPrintFixedAllocationResultRequiresSettledType(t *testing.T) {
	pending := errors.New("allocation pending")
	var output bytes.Buffer
	printFixedAllocationResult(&output, pending)
	if output.Len() != 0 {
		t.Fatalf("pending rejection projected: %q", output.String())
	}
	settled := &cli.FixedAllocationResult{Schema: "crabbox.fixed-allocation-result.v1", Capability: "fixture-capacity-v1", Provider: "fixture", LeaseID: "cbx_abcdef123456", AttemptNonce: "nonce", Category: "capacity_shortage", Allocation: "settled_nonallocation", Companions: "settled", Cause: pending}
	printFixedAllocationResult(&output, settled)
	const prefix = "crabbox-allocation-result "
	var result cli.FixedAllocationResult
	if !bytes.HasPrefix(output.Bytes(), []byte(prefix)) ||
		json.Unmarshal(bytes.TrimSpace(output.Bytes()[len(prefix):]), &result) != nil ||
		result.Schema != "crabbox.fixed-allocation-result.v1" || result.Capability != settled.Capability ||
		result.LeaseID != settled.LeaseID || result.AttemptNonce != settled.AttemptNonce ||
		result.Category != "capacity_shortage" || result.Allocation != "settled_nonallocation" || result.Companions != "settled" {
		t.Fatalf("unexpected typed projection: %q", output.String())
	}
}
