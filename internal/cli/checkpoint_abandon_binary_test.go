//go:build !windows

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCheckpointAbandonContract(t *testing.T, repo, binary string) {
	t.Run("abandon legacy source preserves unknown image obligation", func(t *testing.T) {
		f := newCheckpointAbandonFixture(t, repo, binary)
		finishCheckpointAbandonment(t, f, captureFixtureCheckpoint)
		before, _ := json.Marshal(f.state())
		finishCheckpointAbandonment(t, f, captureFixtureCheckpoint)
		after, _ := json.Marshal(f.state())
		if !bytes.Equal(before, after) {
			t.Fatal("terminal abandonment repeated a native effect")
		}
		wrongSource := checkpointAbandonArgs(captureFixtureCheckpoint)
		wrongSource[len(wrongSource)-2] = "replacement-resource"
		if result := f.run(wrongSource...); result.err == nil {
			t.Fatal("terminal replay ignored a different expected source")
		}
		record := f.record(captureFixtureCheckpoint)
		if record.Capture.SourceDisposition != "abandon" || record.Capture.Error == "" || record.Native.ImageID != "" || record.Native.Resource != "" {
			t.Fatalf("source cleanup erased image uncertainty or invented an image: %+v", record)
		}
		if record.Native.Metadata["machine0_source_cleanup_account_id"] != "fixture-account" || record.Native.Metadata["machine0_account_id"] != "" {
			t.Fatalf("source cleanup invented historical image account authority: %+v", record.Native.Metadata)
		}
		for _, args := range [][]string{
			{"checkpoint", "inspect", captureFixtureCheckpoint, "--verify", "--json"},
			{"checkpoint", "list", "--verify", "--json"},
		} {
			result := f.run(args...)
			var audits []checkpointAudit
			var decodeErr error
			if args[1] == "inspect" {
				audits = make([]checkpointAudit, 1)
				decodeErr = json.Unmarshal(result.stdout, &audits[0])
			} else {
				decodeErr = json.Unmarshal(result.stdout, &audits)
			}
			if result.err != nil || decodeErr != nil || len(audits) != 1 {
				t.Fatalf("catalog lost unresolved abandoned image: %v\n%s\n%s", result.err, result.stdout, result.stderr)
			}
			audit := audits[0]
			if audit.Record.ID != captureFixtureCheckpoint || audit.Record.Capture == nil || audit.Record.Capture.Phase != "abandoned" || audit.NextAction != "reconcile_image" || audit.Error == "" {
				t.Fatalf("catalog concealed unresolved image after source cleanup: %+v", audit)
			}
		}
		requireAbandonedCheckpointHeld(t, f, captureFixtureCheckpoint)
	})

	for _, refusal := range []string{"wrong expected source", "missing expected source", "source absent", "other account absent", "source replacement", "another unresolved checkpoint", "existing retirement"} {
		t.Run("abandon refuses "+refusal, func(t *testing.T) {
			f := newCheckpointAbandonFixture(t, repo, binary)
			args := checkpointAbandonArgs(captureFixtureCheckpoint)
			s := f.state()
			switch refusal {
			case "wrong expected source":
				args[len(args)-2] = "replacement-resource"
			case "missing expected source":
				args = append(args[:len(args)-3], "--json")
			case "source absent", "other account absent":
				s.Machine = nil
				if refusal == "other account absent" {
					s.AccountID = "other-account"
				}
			case "source replacement":
				s.Machine["id"] = "replacement-resource"
			case "another unresolved checkpoint":
				record := f.record(captureFixtureCheckpoint)
				record.ID = "chk_0123456789abcdef"
				writeAbandonCheckpointRecord(t, f, record)
			case "existing retirement":
				record := f.record(captureFixtureCheckpoint)
				record.Capture = &NativeCheckpointCapture{SourceDisposition: "retire", Phase: "prepared"}
				writeAbandonCheckpointRecord(t, f, record)
			}
			f.writeState(s)
			claimBefore, _ := json.Marshal(f.claim())
			recordBefore, _ := json.Marshal(f.record(captureFixtureCheckpoint))
			result := f.run(args...)
			claimAfter, _ := json.Marshal(f.claim())
			recordAfter, _ := json.Marshal(f.record(captureFixtureCheckpoint))
			if result.err == nil || !bytes.Equal(claimBefore, claimAfter) || !bytes.Equal(recordBefore, recordAfter) {
				t.Fatalf("refusal changed ownership: err=%v claimChanged=%t recordChanged=%t stderr=%s", result.err, !bytes.Equal(claimBefore, claimAfter), !bytes.Equal(recordBefore, recordAfter), result.stderr)
			}
			if after, _ := json.Marshal(f.state()); beforeState(t, s) != string(after) {
				t.Fatalf("refused abandonment changed native resources: %s", after)
			}
		})
	}

	for _, replacement := range []string{"source", "claim generation", "account", "account and absence"} {
		t.Run("abandon replay refuses changed "+replacement, func(t *testing.T) {
			f := newCheckpointAbandonFixture(t, repo, binary)
			s := f.state()
			s.Machine["status"] = "STOPPING"
			f.writeState(s)
			result := f.run(checkpointAbandonArgs(captureFixtureCheckpoint)...)
			record := f.record(captureFixtureCheckpoint)
			if result.err != nil || record.Capture == nil || record.Capture.Phase != "retiring" || f.state().Removes != 0 {
				t.Fatalf("transitional source did not retain abandonment binding: err=%v record=%+v stderr=%s", result.err, record, result.stderr)
			}
			s = f.state()
			s.Machine["status"] = "ERRORED"
			switch replacement {
			case "source":
				s.Machine["id"] = "replacement-resource"
			case "claim generation":
				claim := f.claim()
				claim.Revision = "replacement-claim-generation"
				f.writeJSON(filepath.Join(f.root, "state", "crabbox", "claims", captureFixtureLease+".json"), claim)
			case "account", "account and absence":
				s.AccountID = "other-account"
				if replacement == "account and absence" {
					s.Machine = nil
				}
			}
			f.writeState(s)
			claimBefore, _ := json.Marshal(f.claim())
			result = f.run(checkpointAbandonArgs(captureFixtureCheckpoint)...)
			claimAfter, _ := json.Marshal(f.claim())
			if result.err == nil || !bytes.Equal(claimBefore, claimAfter) || f.record(captureFixtureCheckpoint).Capture.Phase == "abandoned" {
				t.Fatalf("replacement completed source cleanup: err=%v claimChanged=%t stderr=%s", result.err, !bytes.Equal(claimBefore, claimAfter), result.stderr)
			}
			if after, _ := json.Marshal(f.state()); beforeState(t, s) != string(after) {
				t.Fatalf("replacement authorized native mutation: %s", after)
			}
		})
	}

	for _, boundary := range []string{"bound", "removed"} {
		t.Run("abandon survives death after "+boundary, func(t *testing.T) {
			f := newCheckpointAbandonFixture(t, repo, binary)
			s := f.state()
			s.Pause = "get"
			if boundary == "removed" {
				s.Pause = "removed"
			}
			f.writeState(s)
			p := f.start("", checkpointAbandonArgs(captureFixtureCheckpoint)...)
			if boundary == "bound" {
				for {
					f.waitEvent(p, "get")
					record := f.record(captureFixtureCheckpoint)
					if record.Capture != nil && f.claim().CheckpointCapture != nil {
						break
					}
					releaseCheckpointAbandonResponse(t, f)
				}
			} else {
				f.waitEvent(p, "removed")
			}
			f.kill(p)
			record := f.record(captureFixtureCheckpoint)
			if record.Capture == nil || record.Capture.Phase == "abandoned" || record.Capture.SourceID != captureFixtureSource {
				t.Fatalf("interruption lost cleanup identity or falsely completed: %+v", record)
			}
			s = f.state()
			s.Pause = ""
			f.writeState(s)
			finishCheckpointAbandonment(t, f, captureFixtureCheckpoint)
			requireAbandonedCheckpointHeld(t, f, captureFixtureCheckpoint)
		})
	}

	t.Run("abandon reopens prepared journal before claim binding", func(t *testing.T) {
		f := newCheckpointAbandonFixture(t, repo, binary)
		originalClaim := f.claim()
		s := f.state()
		s.Machine["status"] = "STOPPING"
		f.writeState(s)
		result := f.run(checkpointAbandonArgs(captureFixtureCheckpoint)...)
		record := f.record(captureFixtureCheckpoint)
		if result.err != nil || record.Capture == nil || record.Capture.Phase != "retiring" || record.Capture.SourceRevision != originalClaim.Revision {
			t.Fatalf("could not obtain real abandonment receipt: err=%v record=%+v stderr=%s", result.err, record, result.stderr)
		}
		// Recreate the durable state between journal publication and claim binding
		// from the real receipt; there is no native subprocess at this write boundary.
		record.Capture.Phase = "prepared"
		writeAbandonCheckpointRecord(t, f, record)
		f.writeJSON(filepath.Join(f.root, "state", "crabbox", "claims", captureFixtureLease+".json"), originalClaim)
		s.Machine["status"] = "ERRORED"
		f.writeState(s)
		finishCheckpointAbandonment(t, f, captureFixtureCheckpoint)
		requireAbandonedCheckpointHeld(t, f, captureFixtureCheckpoint)
	})

	t.Run("abandon retains image arriving after source cleanup", func(t *testing.T) {
		f := newCheckpointAbandonFixture(t, repo, binary)
		finishCheckpointAbandonment(t, f, captureFixtureCheckpoint)
		s := f.state()
		s.Image = map[string]any{"id": "late-image", "name": "crabbox-" + strings.TrimPrefix(captureFixtureCheckpoint, "chk_"), "status": "DRAFT"}
		s.Metadata = map[string]string{"crabbox_checkpoint": captureFixtureCheckpoint, "crabbox_lease": captureFixtureLease, "crabbox_source": captureFixtureSource}
		s.Ready = true
		f.writeState(s)
		finishCheckpointAbandonment(t, f, captureFixtureCheckpoint)
		requireAbandonedCheckpointHeld(t, f, captureFixtureCheckpoint)
		if after, _ := json.Marshal(f.state()); beforeState(t, s) != string(after) {
			t.Fatalf("late image was changed or deleted: %s", after)
		}
		if record := f.record(captureFixtureCheckpoint); record.Native.ImageID != "" || record.Native.Resource != "" || record.Capture.Error == "" {
			t.Fatalf("late image erased historical uncertainty: %+v", record)
		}
	})

	t.Run("abandon source after lost ordinary save response retains image", func(t *testing.T) {
		f := newCheckpointCaptureFixture(t, repo, binary)
		s := f.state()
		s.Pause = "saved"
		f.writeState(s)
		p := f.start("", "checkpoint", "create", "--provider", "machine0", "--id", captureFixtureLease, "--mode", "native", "--wait=false", "--json")
		f.waitEvent(p, "saved")
		f.kill(p)
		paths, err := filepath.Glob(filepath.Join(f.root, "state", "crabbox", "checkpoints", "*", checkpointMetaFile))
		if err != nil || len(paths) != 1 {
			t.Fatalf("interrupted ordinary save lost reservation: paths=%v err=%v", paths, err)
		}
		id := filepath.Base(filepath.Dir(paths[0]))
		if record := f.record(id); record.Capture != nil || record.Native.ImageID != "" {
			t.Fatalf("fixture did not lose image observation: %+v", record)
		}
		s = f.state()
		s.Pause = ""
		f.writeState(s)
		imageBefore, _ := json.Marshal(s.Image)
		finishCheckpointAbandonment(t, f, id)
		imageAfter, _ := json.Marshal(f.state().Image)
		if f.state().Saves != 1 || !bytes.Equal(imageBefore, imageAfter) || f.record(id).Native.ImageID != "" {
			t.Fatalf("abandonment replayed save or resolved uncertain image: state=%+v record=%+v", f.state(), f.record(id))
		}
		requireAbandonedCheckpointHeld(t, f, id)
	})
}

func newCheckpointAbandonFixture(t *testing.T, repo, binary string) *checkpointCaptureFixture {
	t.Helper()
	f := newCheckpointCaptureFixture(t, repo, binary)
	record := checkpointRecord{ID: captureFixtureCheckpoint, Kind: checkpointKindMachine0, Provider: "machine0", LeaseID: captureFixtureLease, CreatedAt: "2026-01-01T00:00:00Z", LastUsedAt: "2026-01-01T00:00:00Z"}
	record.Repo.Root, record.Repo.Name = repo, filepath.Base(repo)
	writeAbandonCheckpointRecord(t, f, record)
	return f
}

func writeAbandonCheckpointRecord(t *testing.T, f *checkpointCaptureFixture, record checkpointRecord) {
	t.Helper()
	path := filepath.Join(f.root, "state", "crabbox", "checkpoints", record.ID, checkpointMetaFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f.writeJSON(path, record)
}

func checkpointAbandonArgs(id string) []string {
	return []string{"checkpoint", "abandon", id, "--provider", "machine0", "--id", captureFixtureLease, "--expected-provider-resource-id", captureFixtureSource, "--json"}
}

func finishCheckpointAbandonment(t *testing.T, f *checkpointCaptureFixture, id string) {
	t.Helper()
	before := f.state()
	for attempt := 0; attempt < 3; attempt++ {
		result := f.run(checkpointAbandonArgs(id)...)
		if result.err != nil {
			t.Fatalf("exact legacy source cannot be abandoned: %v\n%s\n%s", result.err, result.stdout, result.stderr)
		}
		var record checkpointRecord
		if err := json.Unmarshal(result.stdout, &record); err != nil || record.ID != id || record.Capture == nil || record.Capture.SourceID != captureFixtureSource {
			t.Fatalf("abandonment lost its exact durable identity: record=%+v decode=%v output=%s", record, err, result.stdout)
		}
		if record.Capture.Phase == "abandoned" {
			state, claim := f.state(), f.claim()
			if state.Machine != nil || state.Removes != 1 || state.Saves != before.Saves || state.Starts != before.Starts || state.Stops != before.Stops || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "released" {
				t.Fatalf("abandonment lost source finalization or mutated image lifecycle: state=%+v claim=%+v", state, claim)
			}
			return
		}
	}
	t.Fatal("source removal did not settle within bounded abandonment replay")
}

func requireAbandonedCheckpointHeld(t *testing.T, f *checkpointCaptureFixture, id string) {
	t.Helper()
	for _, args := range [][]string{
		{"checkpoint", "fork", id, "--dry-run"},
		{"checkpoint", "delete", id},
		{"checkpoint", "delete", id, "--local-only"},
		{"checkpoint", "prune", "--older-than", "1ns"},
		{"checkpoint", "create", "--provider", "machine0", "--id", captureFixtureLease, "--checkpoint-id", id, "--retire-source", "--mode", "native", "--wait=false", "--json"},
	} {
		result := f.run(args...)
		if result.err == nil {
			t.Fatalf("%v erased the unknown image obligation or reused abandonment: %s", args, result.stdout)
		}
		if record := f.record(id); record.Native.ImageID != "" || record.Capture == nil || record.Capture.SourceDisposition != "abandon" || record.Capture.Phase != "abandoned" {
			t.Fatalf("%v changed retained unknown image: %+v", args, record)
		}
	}
}

func releaseCheckpointAbandonResponse(t *testing.T, f *checkpointCaptureFixture) {
	t.Helper()
	gate, err := os.OpenFile(filepath.Join(f.root, "withheld-response"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = gate.Write([]byte{1})
	_ = gate.Close()
	if err != nil {
		t.Fatal(err)
	}
}

func beforeState(t *testing.T, value checkpointCaptureFixtureState) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
