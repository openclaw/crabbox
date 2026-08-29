//go:build !windows

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func runCheckpointCaptureReviewContract(t *testing.T, repo, binary string) {
	t.Run("retirement admission is read only and holds existing operations", func(t *testing.T) {
		f := newCheckpointCaptureFixture(t, repo, binary)
		description := f.run("providers", "describe", "machine0", "--json")
		if description.err != nil || !bytes.Contains(description.stdout, []byte(`"checkpoint-retirement-prepare"`)) {
			t.Fatalf("retirement admission not advertised: %v %s", description.err, description.stdout)
		}
		before, _ := json.Marshal(f.claim())
		for attempt := 0; attempt < 2; attempt++ {
			result := f.run(append(f.retireArgs(), "--prepare-only")...)
			var admission checkpointCaptureAdmission
			if err := json.Unmarshal(result.stdout, &admission); result.err != nil || err != nil || admission.Admission != "ready" || admission.SourceID != captureFixtureSource || admission.LeaseID != captureFixtureLease || admission.ID != captureFixtureCheckpoint || admission.SourceDisposition != "retire" {
				t.Fatalf("admission=%+v err=%v decode=%v stderr=%s", admission, result.err, err, result.stderr)
			}
		}
		after, _ := json.Marshal(f.claim())
		_, journalErr := os.Stat(filepath.Join(f.root, "state", "crabbox", "checkpoints", captureFixtureCheckpoint, checkpointMetaFile))
		if !bytes.Equal(before, after) || !os.IsNotExist(journalErr) || f.state().Stops != 0 || f.state().Saves != 0 {
			t.Fatal("read-only admission committed ownership or changed the source")
		}
		f.requirePending()
		if result := f.run(append(f.retireArgs(), "--prepare-only")...); result.err == nil {
			t.Fatal("admission treated an existing submitted operation as unsubmitted")
		}
		s := f.state()
		s.Ready = true
		f.writeState(s)
		f.finishRetirement()
	})

	t.Run("readiness probe cannot restart pending capture", func(t *testing.T) {
		f := newCheckpointCaptureFixture(t, repo, binary)
		f.requirePending()
		s := f.state()
		s.StartRunning = true
		f.writeState(s)
		before, _ := json.Marshal(f.claim())
		if read := f.run("status", "--provider", "machine0", "--id", captureFixtureLease, "--json"); read.err != nil {
			t.Fatalf("read-only status rejected a held source: %v %s", read.err, read.stderr)
		}
		result := f.run("status", "--provider", "machine0", "--id", captureFixtureLease, "--wait", "--json")
		after, _ := json.Marshal(f.claim())
		if result.err == nil || f.state().Starts != 0 || !bytes.Equal(before, after) {
			t.Fatalf("readiness bypassed capture: err=%v starts=%d claimChanged=%t stderr=%s", result.err, f.state().Starts, !bytes.Equal(before, after), result.stderr)
		}
		s = f.state()
		s.Ready = true
		f.writeState(s)
		f.finishRetirement()
	})

	for _, policy := range []string{"configured", "claimed"} {
		t.Run(policy+" suspend rejection leaves source usable", func(t *testing.T) {
			f := newCheckpointCaptureFixture(t, repo, binary)
			if policy == "configured" {
				config := filepath.Join(f.root, "config.yaml")
				data, err := os.ReadFile(config)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(config, append(data, []byte("  releasePolicy: suspend\n")...), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				claim := f.claim()
				claim.Labels["release_policy"] = "suspend"
				f.writeJSON(filepath.Join(f.root, "state", "crabbox", "claims", captureFixtureLease+".json"), claim)
			}
			before, _ := json.Marshal(f.claim())
			prepare := f.run(append(f.retireArgs(), "--prepare-only")...)
			var admission checkpointCaptureAdmission
			if err := json.Unmarshal(prepare.stdout, &admission); prepare.err != nil || err != nil || admission.Admission != "unsupported" || admission.SourceID != captureFixtureSource || admission.Reason == "" {
				t.Fatalf("policy refusal lacked a producer admission receipt: %+v err=%v decode=%v stderr=%s", admission, prepare.err, err, prepare.stderr)
			}
			for attempt := 0; attempt < 2; attempt++ {
				result := f.run(f.retireArgs()...)
				if result.err == nil {
					t.Fatal("suspend policy admitted retirement")
				}
			}
			after, _ := json.Marshal(f.claim())
			records, err := filepath.Glob(filepath.Join(f.root, "state", "crabbox", "checkpoints", "*", checkpointMetaFile))
			if err != nil || len(records) != 0 || !bytes.Equal(before, after) {
				t.Errorf("rejection poisoned ownership: records=%v claimChanged=%t err=%v", records, !bytes.Equal(before, after), err)
			}
			if s := f.state(); s.Stops != 0 || s.Saves != 0 || s.Starts != 0 || s.Removes != 0 {
				t.Fatalf("rejection mutated source: %+v", s)
			}
			command := "pause"
			if policy == "configured" {
				command = "stop"
			}
			result := f.run(command, "--provider", "machine0", captureFixtureLease)
			if result.err != nil || f.state().Machine == nil || f.state().Machine["status"] != "SUSPENDED" || f.state().Removes != 0 || f.claim().CloudID != captureFixtureSource {
				t.Fatalf("unchanged suspend source became unusable: %v %s %+v", result.err, result.stderr, f.state())
			}
		})
	}

	for _, action := range []string{"delete", "delete-local", "prune"} {
		t.Run("ordinary writer serializes "+action, func(t *testing.T) {
			f := newCheckpointCaptureFixture(t, repo, binary)
			s := f.state()
			s.StartRunning = true
			f.writeState(s)
			writer := f.start("waiting image=", "checkpoint", "create", "--provider", "machine0", "--id", captureFixtureLease, "--mode", "native", "--wait=false", "--json")
			writer.waitMarker(t)
			records, err := filepath.Glob(filepath.Join(f.root, "state", "crabbox", "checkpoints", "*", checkpointMetaFile))
			if err != nil || len(records) != 1 {
				t.Fatalf("records=%v err=%v", records, err)
			}
			id := filepath.Base(filepath.Dir(records[0]))
			if f.record(id).Native.ImageID == "" {
				t.Fatal("test did not reach observed image publication")
			}
			lockPath := filepath.Join(f.root, "state", "crabbox", "checkpoints", id+".lock")
			lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			held := err == syscall.EWOULDBLOCK
			if err == nil {
				_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			} else if !held {
				t.Fatal(err)
			}
			if !held {
				t.Error("ordinary writer did not hold the canonical checkpoint lock after image publication")
			}
			args := []string{"checkpoint", "delete", id}
			if action == "delete-local" {
				args = append(args, "--local-only")
			}
			if action == "prune" {
				args = []string{"checkpoint", "prune", "--older-than", "1ns"}
			}
			deleter := f.start("", args...)
			// On the broken writer, finish the intervening deletion first to
			// reproduce directory resurrection. The fixed writer owns the lock;
			// its cancellation/restore must finish before deletion can proceed.
			if !held {
				<-deleter.done
			}
			if err := writer.signalGroup(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			<-writer.done
			<-deleter.done
			if writer.err == nil || deleter.err != nil {
				t.Fatalf("writer=%v deleter=%v %s", writer.err, deleter.err, deleter.output.stderr.String())
			}
			if _, err := os.Stat(records[0]); !os.IsNotExist(err) {
				t.Fatalf("ordinary writer recreated deleted checkpoint: %v", err)
			}
			if s := f.state(); s.Saves != 1 || s.Starts != 1 || s.Machine["status"] != "RUNNING" {
				t.Fatalf("ordinary restore changed: %+v", s)
			}
			if action != "delete-local" {
				f.requireOrder("started", "image-removed")
			}
		})
	}
}
