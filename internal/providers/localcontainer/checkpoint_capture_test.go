package localcontainer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCheckpointSourceAbsenceReadsUnfilteredContainerIDs(t *testing.T) {
	for _, state := range []string{"present", "absent", "error", "invalid", "wrong-daemon"} {
		t.Run(state, func(t *testing.T) {
			id := strings.Repeat("a", 64)
			output, code, daemon := "", 0, "fixture-daemon"
			switch state {
			case "present":
				output = id
			case "error":
				code = 1
			case "invalid":
				output = "truncated-id"
			case "wrong-daemon":
				daemon = "replacement-daemon"
			}
			runtime := filepath.Join(t.TempDir(), "docker")
			script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  "info --format {{.ID}}") printf '%%s\n' '%s' ;;
  "ps -a --no-trunc --format {{.ID}}") printf '%%s\n' '%s'; exit %d ;;
  *) exit 2 ;;
esac
`, daemon, output, code)
			if err := os.WriteFile(runtime, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			b := &backend{}
			absent, err := b.CheckpointSourceAbsent(context.Background(), core.CheckpointSourceRequest{Capture: core.NativeCheckpointCapture{SourceID: id}, Resource: core.NativeCheckpointResourceRequest{Metadata: map[string]string{checkpointMetadataRuntime: runtime, checkpointMetadataDaemonID: "fixture-daemon"}}})
			if absent != (state == "absent") || (err != nil) != (state == "error" || state == "invalid" || state == "wrong-daemon") {
				t.Fatalf("absent=%v err=%v", absent, err)
			}
		})
	}
}

func TestCheckpointReservationHoldsClaimlessContainerCleanup(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprint(dryRun), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const id = "cbx_abcdef123456"
			dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "checkpoints", "chk_capture_hold")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			const journal = `{"id":"chk_capture_hold","provider":"local-container","kind":"docker-commit","leaseId":"cbx_abcdef123456","capture":{"sourceDisposition":"retire","phase":"pending"}}`
			path := filepath.Join(dir, "checkpoint.json")
			if err := os.WriteFile(path, []byte(journal), 0600); err != nil {
				t.Fatal(err)
			}
			runner := &recordingRunner{}
			_, err := testBackend(runner).cleanupClaimlessContainer(context.Background(), id, testRecoveredContainerID, nil, dryRun)
			if err == nil || len(runner.calls) != 0 {
				t.Fatalf("claimless capture was not held: err=%v commands=%+v", err, runner.commandSummary())
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != journal {
				t.Fatal("cleanup changed unresolved journal")
			}
		})
	}
}
