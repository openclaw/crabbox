//go:build !windows

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func runCheckpointReadinessOverlapContract(t *testing.T, repo, binary string) {
	for _, phase := range []string{"submitted", "reserved before binding"} {
		t.Run("readiness overlaps "+phase+" capture", func(t *testing.T) {
			f := newCheckpointCaptureFixture(t, repo, binary)
			s := f.state()
			s.Pause, s.StartRunning = "get", true
			f.writeState(s)
			status := f.start("", "status", "--provider", "machine0", "--id", captureFixtureLease, "--wait", "--json")
			f.waitEvent(status, "get")
			s.Pause = ""
			f.writeState(s)
			if phase == "submitted" {
				// Actual create reserves, binds, stops and submits while status's
				// native read is in flight, after its first authorization passed.
				f.requirePending()
			} else {
				// The owning reservation can survive without a new claim revision.
				// Exercise that real store/claim boundary, not a synthetic marker.
				t.Setenv("XDG_STATE_HOME", filepath.Join(f.root, "state"))
				claim := f.claim()
				record := checkpointRecord{ID: captureFixtureCheckpoint, Kind: checkpointKindMachine0, Provider: "machine0", LeaseID: captureFixtureLease}
				record.Repo.Root, record.Repo.Name = repo, filepath.Base(repo)
				record.Native.Strategy, record.Native.NoReboot = CheckpointStrategyImage, true
				record.Workdir = "/home/ubuntu/crabbox/" + captureFixtureLease + "/" + filepath.Base(repo)
				record.Capture = &NativeCheckpointCapture{SourceDisposition: "retire", Phase: "prepared", SourceID: claim.CloudID, SourceName: claim.Labels["machine0_name"], SourceScope: claim.ProviderScope, SourceRevision: claim.Revision, SourceClaimedAt: claim.ClaimedAt, SourceIntent: claim.FixedCreateIntent.Fingerprint}
				store := checkpointStore{root: filepath.Join(f.root, "state", "crabbox", "checkpoints")}
				if err := store.WithLock(record.ID, func() error {
					_, _, err := reserveSourceCheckpoint(store, record, claim)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if current := f.claim(); current.Revision != claim.Revision || current.CheckpointCapture != nil {
					t.Fatal("test reservation changed the claim before binding")
				}
				s = f.state()
				s.Machine["status"] = "STOPPED"
				f.writeState(s)
			}
			before, _ := json.Marshal(f.claim())
			gate, err := os.OpenFile(filepath.Join(f.root, "withheld-response"), os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = gate.Write([]byte{1})
			_ = gate.Close()
			if err != nil {
				t.Fatal(err)
			}
			// A broken producer starts before SSH probing; stop that invocation
			// at the observed mutation rather than waiting for a fixture timeout.
		waitStatus:
			for {
				select {
				case <-status.done:
					break waitStatus
				case event := <-f.events:
					if event.Owner == status.cmd.Process.Pid && event.Kind == "started" {
						f.kill(status)
						break waitStatus
					}
				}
			}
			after, _ := json.Marshal(f.claim())
			if status.err == nil || f.state().Starts != 0 || !bytes.Equal(before, after) {
				t.Fatalf("overlapping readiness bypassed current capture: err=%v starts=%d claimChanged=%t", status.err, f.state().Starts, !bytes.Equal(before, after))
			}
			s = f.state()
			s.Ready = true
			f.writeState(s)
			f.finishRetirement()
			if s := f.state(); s.Saves != 1 || s.Starts != 0 || s.Removes != 1 {
				t.Fatalf("overlap damaged same-operation replay: %+v", s)
			}
		})
	}
}
