//go:build !windows

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func runCheckpointMachine0StrategyContract(t *testing.T, repo, binary string) {
	for _, strategy := range []string{"auto", "image", "disk-snapshot"} {
		t.Run("Machine0 retirement strategy "+strategy, func(t *testing.T) {
			f := newCheckpointCaptureFixture(t, repo, binary)
			args := append(f.retireArgs(), "--strategy", strategy)
			claimPath := filepath.Join(f.root, "state", "crabbox", "claims", captureFixtureLease+".json")
			before, err := os.ReadFile(claimPath)
			if err != nil {
				t.Fatal(err)
			}
			admission := f.run(append(append([]string{}, args...), "--prepare-only")...)
			wantAdmission := "ready"
			if strategy == "disk-snapshot" {
				wantAdmission = "unsupported"
			}
			var receipt checkpointCaptureAdmission
			if err := json.Unmarshal(admission.stdout, &receipt); err != nil || admission.err != nil || receipt.Admission != wantAdmission || receipt.SourceID != captureFixtureSource {
				t.Fatalf("native admission=%+v want=%s err=%v json=%v\n%s", receipt, wantAdmission, admission.err, err, admission.stderr)
			}
			result := f.run(args...)
			if strategy == "disk-snapshot" {
				after, readErr := os.ReadFile(claimPath)
				_, journalErr := os.Stat(filepath.Join(f.root, "state", "crabbox", "checkpoints", captureFixtureCheckpoint, checkpointMetaFile))
				state := f.state()
				if result.err == nil || readErr != nil || !bytes.Equal(before, after) || !os.IsNotExist(journalErr) || state.Stops != 0 || state.Saves != 0 || state.Starts != 0 || state.Removes != 0 {
					t.Fatalf("unsupported explicit strategy changed ownership/source: err=%v stderr=%s journal=%v state=%+v", result.err, result.stderr, journalErr, state)
				}
				return
			}
			if result.err != nil {
				t.Fatalf("native capture failed: %v\n%s", result.err, result.stderr)
			}
			record := f.record(captureFixtureCheckpoint)
			if record.Kind != checkpointKindMachine0 || record.Native.Strategy != checkpointStrategyImage || record.Capture == nil || record.Capture.Phase != "pending" || record.Capture.StrategyExplicit != (strategy == "image") || record.Native.ImageID == "" {
				t.Fatalf("pending selection was not retained: %+v", record)
			}
			state := f.state()
			state.Ready = true
			f.writeState(state)
			for replay := 0; replay < 3; replay++ {
				result = f.run(args...)
				if result.err != nil {
					t.Fatalf("same-strategy replay failed: %v\n%s", result.err, result.stderr)
				}
			}
			final, state := f.record(captureFixtureCheckpoint), f.state()
			if final.Capture.Phase != "retired" || final.Native.ImageID != record.Native.ImageID || state.Saves != 1 || state.Starts != 0 || state.Removes != 1 || state.Machine != nil {
				t.Fatalf("retirement did not retain exact operation: record=%+v state=%+v", final, state)
			}
		})
	}
}
