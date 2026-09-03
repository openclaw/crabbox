//go:build !windows

package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const captureFixtureEnv = "CRABBOX_CAPTURE_BINARY_FIXTURE"
const captureFixtureLease = "cbx_abcdef012345"
const captureFixtureCheckpoint = "chk_abcdef0123456789"
const captureFixtureSource = "fixture-source-immutable-id"

// These tests cross the actual CLI, native subprocess, claim, and checkpoint
// boundaries. Cooperative context cancellation alone misses the lost record
// while Machine0's source restart outlives the caller's cancellation.
func TestCheckpointCaptureBuiltBinaryContract(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	repo, err = filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := builtCLITestBinary()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("compiled immutable Crabbox binary: %s", binary)
	providerBuild := exec.Command("go", "build", "-trimpath", "-o", binary+".provider", "./internal/cli/testdata/checkpoint-provider")
	providerBuild.Dir = repo
	if output, err := providerBuild.CombinedOutput(); err != nil {
		t.Fatalf("build fake provider: %v\n%s", err, output)
	}

	runCheckpointAbandonContract(t, repo, binary)

	for _, boundary := range []string{"flush", "stopped"} {
		t.Run("ordinary failure before submission releases reservation after "+boundary, func(t *testing.T) {
			t.Parallel()
			f := newCheckpointCaptureFixture(t, repo, binary)
			s := f.state()
			s.Pause, s.StartRunning = boundary, true
			f.writeState(s)
			p := f.start("", "checkpoint", "create", "--provider", "machine0", "--id", captureFixtureLease, "--mode", "native", "--wait=false", "--json")
			f.waitEvent(p, boundary)
			s = f.state()
			s.Pause, s.Machine["status"] = "", "ERRORED"
			f.writeState(s)
			gate, err := os.OpenFile(filepath.Join(f.root, "withheld-response"), os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = gate.Write([]byte{1})
			_ = gate.Close()
			if err != nil {
				t.Fatal(err)
			}
			<-p.done
			s = f.state()
			wantTransitions := 0
			if boundary == "stopped" {
				wantTransitions = 1
			}
			if p.err == nil || s.Saves != 0 || s.Stops != wantTransitions || s.Starts != wantTransitions || s.Removes != 0 {
				t.Fatalf("did not reproduce failure before image submission: err=%v stderr=%s state=%+v", p.err, p.output.stderr.String(), s)
			}
			result := f.run("stop", "--provider", "machine0", captureFixtureLease)
			if result.err != nil || f.state().Machine != nil || f.state().Removes != 1 {
				t.Fatalf("known unsubmitted capture blocked source release: err=%v stderr=%s state=%+v", result.err, result.stderr, f.state())
			}
			if claim := f.claim(); claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "released" || claim.CheckpointCapture != nil {
				t.Fatalf("released source retained an active claim: %+v", claim)
			}
		})
	}

	t.Run("ordinary interrupted submission retains blank reservation", func(t *testing.T) {
		t.Parallel()
		f := newCheckpointCaptureFixture(t, repo, binary)
		s := f.state()
		s.Pause = "saved"
		f.writeState(s)
		p := f.start("", "checkpoint", "create", "--provider", "machine0", "--id", captureFixtureLease, "--mode", "native", "--wait=false", "--json")
		f.waitEvent(p, "saved")
		f.kill(p)
		s = f.state()
		s.Pause, s.Image = "", nil
		f.writeState(s)
		records, err := filepath.Glob(filepath.Join(f.root, "state", "crabbox", "checkpoints", "*", checkpointMetaFile))
		if err != nil || len(records) != 1 {
			t.Fatalf("checkpoint reservations=%v err=%v", records, err)
		}
		var record checkpointRecord
		f.readJSON(records[0], &record)
		if record.Native.ImageID != "" {
			t.Fatalf("fixture observed image before interrupted submission: %+v", record)
		}
		for _, args := range [][]string{
			{"stop", "--provider", "machine0", captureFixtureLease},
			{"checkpoint", "delete", record.ID}, {"checkpoint", "delete", record.ID, "--local-only"},
			{"checkpoint", "prune", "--older-than", "1ns"},
		} {
			if result := f.run(args...); result.err == nil {
				t.Fatalf("ambiguous submission permitted %v: %s", args, result.stdout)
			}
			if _, err := os.Stat(records[0]); err != nil {
				t.Fatalf("ambiguous ownership was erased by %v: %v", args, err)
			}
		}
		if s := f.state(); s.Saves != 1 || s.Removes != 0 || s.Machine == nil {
			t.Fatalf("ambiguous submission authorized cleanup: %+v", s)
		}
	})

	t.Run("ordinary capture retains observed image before killed rollback", func(t *testing.T) {
		f := newCheckpointCaptureFixture(t, repo, binary)
		s := f.state()
		s.Pause = "started"
		f.writeState(s)
		f.scrub()
		p := f.start("waiting image=", "checkpoint", "create", "--provider", "machine0", "--id", captureFixtureLease, "--mode", "native", "--wait=false", "--json")
		p.waitMarker(t)
		if err := p.signalGroup(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		grace := time.NewTimer(300 * time.Millisecond)
		defer grace.Stop()
		<-grace.C
		f.kill(p)
		if p.err == nil {
			t.Fatal("killed ordinary capture unexpectedly succeeded")
		}
		s = f.state()
		if s.Saves != 1 || s.Starts != 1 || s.Machine["status"] != "STARTING" {
			t.Fatalf("ordinary cancellation did not reproduce pending snapshot then restart: %+v", s)
		}
		records, err := filepath.Glob(filepath.Join(f.root, "state", "crabbox", "checkpoints", "*", checkpointMetaFile))
		if err != nil || len(records) != 1 {
			t.Fatalf("checkpoint reservations=%v err=%v", records, err)
		}
		var record checkpointRecord
		f.readJSON(records[0], &record)
		if record.Native.ImageID == "" || record.Native.Metadata["machine0_source_machine"] != captureFixtureSource {
			t.Fatalf("observed image was lost before restart: %+v", record)
		}
		if result := f.run("stop", "--provider", "machine0", captureFixtureLease); result.err == nil {
			t.Fatalf("STARTING source was falsely reported destroyed: %s", result.stdout)
		}
		if f.claim().CloudID != captureFixtureSource || f.state().Machine == nil {
			t.Fatal("failed cleanup lost its source/claim ownership")
		}
		f.requireOrder("scrub", "flush", "stopped", "saved", "image", "started")
	})

	for _, boundary := range []string{"stopped", "saved", "image", "removed"} {
		t.Run("retirement survives death after "+boundary, func(t *testing.T) {
			t.Parallel()
			f := newCheckpointCaptureFixture(t, repo, binary)
			s := f.state()
			s.Pause, s.Ready = boundary, boundary == "removed"
			f.writeState(s)
			f.scrub()
			p := f.start("", f.retireArgs()...)
			f.waitEvent(p, boundary)
			f.kill(p)
			record := f.record(captureFixtureCheckpoint)
			if record.Capture == nil || record.Capture.SourceID != captureFixtureSource || record.Capture.SourceRevision == "" || record.Capture.Phase == "retired" {
				t.Fatalf("interrupted operation lost ownership or falsely retired: %+v", record)
			}
			s = f.state()
			s.Pause, s.Ready = "", true
			f.writeState(s)
			f.finishRetirement()
			s = f.state()
			if s.Saves != 1 || s.Starts != 0 || s.Removes != 1 || s.Machine != nil {
				t.Fatalf("replay repeated capture, restarted, or failed retirement: %+v", s)
			}
			// Final replay must remain terminal without repeating a native mutation.
			f.finishRetirement()
			after := f.state()
			if after.Saves != s.Saves || after.Starts != s.Starts || after.Removes != s.Removes {
				t.Fatalf("terminal replay mutated the provider: before=%+v after=%+v", s, after)
			}
			inspection := f.run("checkpoint", "inspect", captureFixtureCheckpoint, "--verify", "--json")
			if inspection.err != nil || !bytes.Contains(inspection.stdout, []byte(`"nextAction":"fork_or_delete"`)) {
				t.Fatalf("configured native CLI context was lost on inspect: %v\n%s\n%s", inspection.err, inspection.stdout, inspection.stderr)
			}
			deletion := f.run("checkpoint", "delete", captureFixtureCheckpoint)
			if deletion.err != nil || f.state().Image != nil {
				t.Fatalf("configured native CLI context was lost on delete: %v\n%s", deletion.err, deletion.stderr)
			}
		})
	}

	t.Run("replacement observed after stop cannot be saved", func(t *testing.T) {
		t.Parallel()
		f := newCheckpointCaptureFixture(t, repo, binary)
		s := f.state()
		s.ReplaceStopped = true
		f.writeState(s)
		result := f.run(f.retireArgs()...)
		if result.err == nil || f.state().Saves != 0 || f.state().Removes != 0 || f.state().Starts != 0 {
			t.Fatalf("observed replacement authorized a mutation: %v\n%s\n%+v", result.err, result.stderr, f.state())
		}
		if record := f.record(captureFixtureCheckpoint); record.Capture == nil || record.Capture.SourceID != captureFixtureSource {
			t.Fatalf("replacement was adopted into operation: %+v", record)
		}
	})

	t.Run("ordinary capture revalidates source after awaited flush", func(t *testing.T) {
		t.Parallel()
		f := newCheckpointCaptureFixture(t, repo, binary)
		s := f.state()
		s.ReplaceAfterFlush = true
		f.writeState(s)
		result := f.run("checkpoint", "create", "--provider", "machine0", "--id", captureFixtureLease, "--mode", "native", "--wait=false", "--json")
		s = f.state()
		if result.err == nil || s.Stops != 0 || s.Saves != 0 || s.Starts != 0 || s.Removes != 0 {
			t.Fatalf("awaited flush authorized mutation of replaced source: err=%v stderr=%s provider=%+v", result.err, result.stderr, s)
		}
	})

	for _, transition := range []string{"STARTING", "STOPPING"} {
		t.Run("retirement holds "+transition, func(t *testing.T) {
			t.Parallel()
			f := newCheckpointCaptureFixture(t, repo, binary)
			s := f.state()
			s.Machine["status"] = transition
			f.writeState(s)
			result := f.run(f.retireArgs()...)
			if result.err != nil {
				t.Fatalf("bounded transition result: %v\n%s", result.err, result.stderr)
			}
			record := f.record(captureFixtureCheckpoint)
			s = f.state()
			if record.Capture == nil || record.Capture.Phase == "retired" || s.Saves != 0 || s.Starts != 0 || s.Removes != 0 {
				t.Fatalf("transition was treated as retired or snapshot safe: record=%+v provider=%+v", record, s)
			}
			s.Machine["status"], s.Ready = "STOPPED", true
			f.writeState(s)
			f.finishRetirement()
		})
	}

	t.Run("lost save response and absent image never authorize another save", func(t *testing.T) {
		t.Parallel()
		f := newCheckpointCaptureFixture(t, repo, binary)
		s := f.state()
		s.Pause = "saved"
		f.writeState(s)
		p := f.start("", f.retireArgs()...)
		f.waitEvent(p, "saved")
		f.kill(p)
		s = f.state()
		s.Pause, s.Image = "", nil
		f.writeState(s)
		for attempt := 0; attempt < 2; attempt++ {
			result := f.run(f.retireArgs()...)
			if result.err != nil {
				t.Fatalf("unresolved submission should return its durable handle: %v\n%s", result.err, result.stderr)
			}
			record := f.record(captureFixtureCheckpoint)
			if record.Capture == nil || record.Capture.Phase != "submitting" || record.Capture.Error == "" {
				t.Fatalf("missing image was treated as unsubmitted: %+v", record)
			}
		}
		if s := f.state(); s.Saves != 1 || s.Starts != 0 || s.Removes != 0 || s.Machine == nil {
			t.Fatalf("ambiguous image absence authorized another effect: %+v", s)
		}
	})

	for _, state := range []string{"STARTING", "STOPPING"} {
		t.Run("ready image retirement waits for source "+state, func(t *testing.T) {
			t.Parallel()
			f := newCheckpointCaptureFixture(t, repo, binary)
			f.requirePending()
			s := f.state()
			s.Ready, s.Machine["status"] = true, state
			f.writeState(s)
			result := f.run(f.retireArgs()...)
			if result.err != nil || f.record(captureFixtureCheckpoint).Capture.Phase != "retiring" || f.state().Removes != 0 {
				t.Fatalf("transitional source was retired: err=%v stderr=%s state=%+v", result.err, result.stderr, f.state())
			}
			s = f.state()
			s.Ready, s.Machine["status"] = false, "STOPPED"
			f.writeState(s)
			result = f.run(f.retireArgs()...)
			if result.err == nil || f.state().Removes != 0 || f.state().Starts != 0 {
				t.Fatalf("stale READY observation authorized source deletion: err=%v state=%+v", result.err, f.state())
			}
			s = f.state()
			s.Ready = true
			f.writeState(s)
			f.finishRetirement()
		})
	}

	t.Run("historical blank reservation is held by capture delete and prune", func(t *testing.T) {
		t.Parallel()
		f := newCheckpointCaptureFixture(t, repo, binary)
		id := "chk_0123456789abcdef"
		path := filepath.Join(f.root, "state", "crabbox", "checkpoints", id, checkpointMetaFile)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		f.writeJSON(path, map[string]any{
			"id": id, "kind": "machine0-image", "provider": "machine0", "leaseId": captureFixtureLease,
			"createdAt": "2026-01-01T00:00:00Z", "lastUsedAt": "2026-01-01T00:00:00Z", "native": map[string]any{},
		})
		for _, args := range [][]string{
			append(f.retireArgs(), "--prepare-only"),
			f.retireArgs(), {"checkpoint", "create", "--provider", "machine0", "--id", captureFixtureLease, "--mode", "native", "--wait=false", "--json"},
			{"checkpoint", "delete", id}, {"checkpoint", "delete", id, "--local-only"},
			{"checkpoint", "prune", "--older-than", "1ns"},
			{"stop", "--provider", "machine0", captureFixtureLease},
			{"status", "--provider", "machine0", "--id", captureFixtureLease, "--wait", "--json"},
		} {
			result := f.run(args...)
			if result.err == nil {
				t.Errorf("historical ambiguity permitted %v: %s", args, result.stdout)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("historical ownership was erased by %v: %v", args, err)
			}
		}
		if s := f.state(); s.Saves != 0 || s.Starts != 0 || s.Removes != 0 {
			t.Fatalf("historical ambiguity authorized native mutation: %+v", s)
		}
	})

	t.Run("configured suspend never becomes destructive retirement", func(t *testing.T) {
		t.Parallel()
		f := newCheckpointCaptureFixture(t, repo, binary)
		config, err := os.OpenFile(filepath.Join(f.root, "config.yaml"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = config.WriteString("  releasePolicy: suspend\n")
		_ = config.Close()
		if err != nil {
			t.Fatal(err)
		}
		result := f.run(f.retireArgs()...)
		s := f.state()
		if result.err == nil || s.Stops != 0 || s.Saves != 0 || s.Starts != 0 || s.Removes != 0 {
			t.Fatalf("explicit suspend policy was broadened to capture/destroy: err=%v stderr=%s provider=%+v", result.err, result.stderr, s)
		}
	})

	t.Run("replaced claim generation cannot replay copied operation binding", func(t *testing.T) {
		t.Parallel()
		f := newCheckpointCaptureFixture(t, repo, binary)
		f.requirePending()
		claim := f.claim()
		claim.Revision = "replacement-claim-generation"
		f.writeJSON(filepath.Join(f.root, "state", "crabbox", "claims", captureFixtureLease+".json"), claim)
		s := f.state()
		s.Ready = true
		f.writeState(s)
		result := f.run(f.retireArgs()...)
		if result.err == nil {
			t.Fatalf("copied operation binding adopted a replacement claim: %s", result.stdout)
		}
		if s := f.state(); s.Saves != 1 || s.Starts != 0 || s.Removes != 0 || s.Machine == nil {
			t.Fatalf("replacement claim authorized a native effect: %+v", s)
		}
	})

	t.Run("concurrent operations cannot claim the same stopped source", func(t *testing.T) {
		t.Parallel()
		f := newCheckpointCaptureFixture(t, repo, binary)
		s := f.state()
		s.Pause = "saved"
		f.writeState(s)
		first := f.start("", f.retireArgs()...)
		f.waitEvent(first, "saved")
		secondArgs := f.retireArgs()
		for i, arg := range secondArgs {
			if arg == captureFixtureCheckpoint {
				secondArgs[i] = "chk_0123456789abcdef"
			}
		}
		second := f.start("", secondArgs...)
		f.kill(first)
		<-second.done
		if second.err == nil {
			t.Fatal("second operation acquired already-held source")
		}
		records, err := filepath.Glob(filepath.Join(f.root, "state", "crabbox", "checkpoints", "*", checkpointMetaFile))
		if err != nil || len(records) != 1 || f.state().Saves != 1 {
			t.Fatalf("concurrent operation duplicated reservation/save: records=%v err=%v provider=%+v", records, err, f.state())
		}
	})

	t.Run("ordinary lifecycle cannot bypass the capture owner", func(t *testing.T) {
		t.Parallel()
		f := newCheckpointCaptureFixture(t, repo, binary)
		f.requirePending()
		for _, args := range [][]string{
			{"stop", "--provider", "machine0", captureFixtureLease},
			{"resume", "--provider", "machine0", captureFixtureLease},
			{"warmup", "--provider", "machine0", "--lease-id", captureFixtureLease, "--keep"},
		} {
			result := f.run(args...)
			// The operation reference distinguishes its owner guard from an
			// unrelated fixture/fingerprint/config error that also exits nonzero.
			if result.err == nil || !bytes.Contains(result.stderr, []byte(captureFixtureCheckpoint)) {
				t.Errorf("%v did not report the owning checkpoint: err=%v stdout=%s stderr=%s", args, result.err, result.stdout, result.stderr)
			}
		}
		_ = f.run("cleanup", "--provider", "machine0")
		if s := f.state(); s.Saves != 1 || s.Starts != 0 || s.Removes != 0 || s.Machine == nil || f.claim().CloudID != captureFixtureSource {
			t.Fatalf("ordinary cleanup or reuse bypassed capture ownership: %+v", s)
		}
	})

	for _, replacement := range []string{"image identity", "image version"} {
		t.Run("replacement of "+replacement+" holds source", func(t *testing.T) {
			t.Parallel()
			f := newCheckpointCaptureFixture(t, repo, binary)
			f.requirePending()
			s := f.state()
			s.Ready = true
			if replacement == "image identity" {
				s.Image["id"] = "fixture-replacement-image-id"
			} else {
				s.Version = 2
			}
			f.writeState(s)
			result := f.run(f.retireArgs()...)
			if result.err == nil || f.record(captureFixtureCheckpoint).Capture.Phase == "retired" {
				t.Fatalf("replacement image was adopted: err=%v stdout=%s", result.err, result.stdout)
			}
			if s := f.state(); s.Saves != 1 || s.Starts != 0 || s.Removes != 0 || s.Machine == nil {
				t.Fatalf("replacement image authorized source mutation: %+v", s)
			}
		})
	}

	t.Run("failed snapshot cleanup survives lost image removal response", func(t *testing.T) {
		t.Parallel()
		f := newCheckpointCaptureFixture(t, repo, binary)
		s := f.state()
		s.Failed = true
		f.writeState(s)
		for attempt := 0; attempt < 2; attempt++ {
			result := f.run(f.retireArgs()...)
			if result.err != nil {
				t.Fatalf("terminal capture must retain actionable receipt: %v\n%s", result.err, result.stderr)
			}
			record := f.record(captureFixtureCheckpoint)
			if record.Capture == nil || record.Capture.Phase != "failed" || record.Capture.Error == "" || record.Native.ImageID == "" {
				t.Fatalf("terminal capture lost exact failure identity: %+v", record)
			}
		}
		if s := f.state(); s.Image == nil || s.Machine == nil || s.Saves != 1 || s.Removes != 0 || s.Starts != 0 {
			t.Fatalf("failed image was discarded without explicit disposition: %+v", s)
		}
		s = f.state()
		s.Pause = "image-removed"
		f.writeState(s)
		p := f.start("", append(f.retireArgs(), "--discard-failed")...)
		f.waitEvent(p, "image-removed")
		f.kill(p)
		record := f.record(captureFixtureCheckpoint)
		if record.Capture == nil || !record.Capture.DiscardFailed || record.Capture.Phase == "retired" || record.Native.ImageID == "" {
			t.Fatalf("lost image removal response erased intent or falsely retired: %+v", record)
		}
		s = f.state()
		s.Pause = ""
		f.writeState(s)
		// The choice is durable: replay does not require repeating the flag.
		f.finishRetirement()
		s = f.state()
		if s.Image != nil || s.Machine != nil || s.Saves != 1 || s.Removes != 1 || s.Starts != 0 || !f.record(captureFixtureCheckpoint).Capture.DiscardFailed {
			t.Fatalf("failed-image replay did not complete its exact cleanup: %+v", s)
		}
	})

	t.Run("failed legacy account remains held before discard", func(t *testing.T) {
		t.Parallel()
		f := newCheckpointCaptureFixture(t, repo, binary)
		f.requirePending()
		s := f.state()
		s.Failed = true
		f.writeState(s)
		result := f.run(f.retireArgs()...)
		if result.err != nil || f.record(captureFixtureCheckpoint).Capture.Phase != "failed" {
			t.Fatalf("reach failed capture: %v %s", result.err, result.stderr)
		}
		record := f.record(captureFixtureCheckpoint)
		delete(record.Native.Metadata, "machine0_account_id")
		f.writeJSON(filepath.Join(f.root, "state", "crabbox", "checkpoints", captureFixtureCheckpoint, checkpointMetaFile), record)
		result = f.run(append(f.retireArgs(), "--discard-failed")...)
		if result.err == nil || !bytes.Contains(result.stderr, []byte("account identity")) {
			t.Fatalf("unbound failed capture was discarded: %v %s", result.err, result.stderr)
		}
		if current := f.state(); current.Image == nil || current.Machine == nil || current.Removes != 0 || current.Starts != 0 {
			t.Fatal("unbound failed capture lost source or image")
		}
	})
	for _, accountCase := range []string{"switched", "unbound legacy", "same account absent"} {
		t.Run("retiring account fence "+accountCase, func(t *testing.T) {
			t.Parallel()
			f := newCheckpointCaptureFixture(t, repo, binary)
			f.requirePending()
			s := f.state()
			s.Ready, s.Machine["status"] = true, "STOPPING"
			f.writeState(s)
			result := f.run(f.retireArgs()...)
			if result.err != nil || f.record(captureFixtureCheckpoint).Capture.Phase != "retiring" {
				t.Fatalf("reach retiring: %v %s", result.err, result.stderr)
			}
			record := f.record(captureFixtureCheckpoint)
			if record.Native.Metadata["machine0_account_id"] != "fixture-account" {
				t.Fatal("account binding lost during image observation")
			}
			if accountCase == "unbound legacy" {
				delete(record.Native.Metadata, "machine0_account_id")
				f.writeJSON(filepath.Join(f.root, "state", "crabbox", "checkpoints", captureFixtureCheckpoint, checkpointMetaFile), record)
			}
			s = f.state()
			s.Machine = nil
			if accountCase == "switched" {
				s.AccountID, s.Image = "other-account", nil
			}
			f.writeState(s)
			claimBefore, _ := json.Marshal(f.claim())
			recordBefore, _ := json.Marshal(f.record(captureFixtureCheckpoint))
			result = f.run(f.retireArgs()...)
			if accountCase == "same account absent" {
				if result.err != nil || f.record(captureFixtureCheckpoint).Capture.Phase != "retired" {
					t.Fatalf("same-account absence: %v %s", result.err, result.stderr)
				}
			} else {
				if result.err == nil || !bytes.Contains(result.stderr, []byte("account identity")) {
					t.Fatalf("unattested account accepted: %v %s", result.err, result.stderr)
				}
				claimAfter, _ := json.Marshal(f.claim())
				recordAfter, _ := json.Marshal(f.record(captureFixtureCheckpoint))
				if !bytes.Equal(claimBefore, claimAfter) || !bytes.Equal(recordBefore, recordAfter) {
					t.Fatal("refused account replay changed durable ownership")
				}
			}
			if current := f.state(); current.Saves != s.Saves || current.Starts != s.Starts || current.Removes != s.Removes {
				t.Fatal("absence replay mutated provider")
			}
		})
	}
	runCheckpointCaptureReviewContract(t, repo, binary)
	runCheckpointReadinessOverlapContract(t, repo, binary)
	runCheckpointContainerReviewContract(t, repo, binary)
	runCheckpointAWSStrategyContract(t, repo, binary)
	t.Run("fixed Machine0 fork replay reads detail version", func(t *testing.T) {
		t.Parallel()
		f := newCheckpointCaptureFixture(t, repo, binary)
		s := f.state()
		s.Ready = true
		f.writeState(s)
		f.finishRetirement()
		const forkID = "cbx_123456abcdef"
		args := []string{"checkpoint", "fork", captureFixtureCheckpoint, "--lease-id", forkID, "--slug", "fixed-fork", "--keep", "--json"}
		var first checkpointForkResult
		for replay := 0; replay < 2; replay++ {
			result := f.run(args...)
			if result.err != nil {
				t.Fatalf("fork invocation %d: %v\n%s\n%s", replay, result.err, result.stdout, result.stderr)
			}
			var fork checkpointForkResult
			if err := json.Unmarshal(result.stdout, &fork); err != nil {
				t.Fatalf("fork JSON: %v\n%s", err, result.stdout)
			}
			s = f.state()
			var claim leaseClaim
			f.readJSON(filepath.Join(f.root, "state", "crabbox", "claims", forkID+".json"), &claim)
			if fork.LeaseID != forkID || claim.CloudID != "fixture-fork-immutable-id" || claim.CloudImmutableID != claim.CloudID || claim.FixedCreateIntent.CheckpointID != captureFixtureCheckpoint || s.Machine["imageVersion"] != float64(1) || s.Creates != 1 {
				t.Fatalf("fork lost exact identity or repeated create: fork=%+v claim=%+v state=%+v", fork, claim, s)
			}
			if replay == 0 {
				first = fork
			} else if fork != first {
				t.Fatalf("replay changed fork result: first=%+v replay=%+v", first, fork)
			}
		}
	})

	runCheckpointMachine0StrategyContract(t, repo, binary)
	runCheckpointNativeLifetimeContract(t, repo, binary)
}

type checkpointCaptureFixtureState struct {
	AccountID         string            `json:"accountId"`
	Machine           map[string]any    `json:"machine"`
	Image             map[string]any    `json:"image"`
	Metadata          map[string]string `json:"metadata"`
	StartRunning      bool              `json:"startRunning"`
	Ready             bool              `json:"ready"`
	Failed            bool              `json:"failed"`
	Pause             string            `json:"pause"`
	ReplaceStopped    bool              `json:"replaceStopped"`
	ReplaceAfterFlush bool              `json:"replaceAfterFlush"`
	Saves             int               `json:"saves"`
	Creates           int               `json:"creates"`
	Stops             int               `json:"stops"`
	Starts            int               `json:"starts"`
	Removes           int               `json:"removes"`
	Version           int               `json:"version"`
}

type checkpointCaptureFixtureEvent struct {
	Kind  string `json:"kind"`
	PID   int    `json:"pid"`
	PGID  int    `json:"pgid"`
	Owner int    `json:"owner"`
}

type checkpointCaptureFixture struct {
	t      *testing.T
	root   string
	repo   string
	binary string
	env    []string
	events chan checkpointCaptureFixtureEvent
}

func newCheckpointCaptureFixture(t *testing.T, repo, binary string) *checkpointCaptureFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &checkpointCaptureFixture{t: t, root: root, repo: repo, binary: binary, events: make(chan checkpointCaptureFixtureEvent, 128)}
	for _, dir := range []string{"home", "config", "cache", "tmp", "bin", "native", "state/crabbox/claims"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, kind := range map[string]string{"native/machine0": "machine0", "bin/ssh": "ssh"} {
		body := "#!/bin/sh\nexec " + shellQuote(binary+".provider") + " -- " + kind + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config := "provider: machine0\nnetwork: public\nmachine0:\n  cliPath: " + filepath.Join(root, "native", "machine0") + "\n  size: large\n  region: eu\n  image: ubuntu-24-04\n"
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	f.env = []string{
		"HOME=" + filepath.Join(root, "home"), "XDG_CONFIG_HOME=" + filepath.Join(root, "config"),
		"XDG_STATE_HOME=" + filepath.Join(root, "state"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"TMPDIR=" + filepath.Join(root, "tmp"), "CRABBOX_CONFIG=" + filepath.Join(root, "config.yaml"),
		"PATH=" + filepath.Join(root, "bin") + ":/usr/bin:/bin", captureFixtureEnv + "=" + root,
		"HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1",
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(captureFixtureLease))
	name := fmt.Sprintf("crabbox-proof-owned-%08x", hash.Sum32())
	// Match the fixed warmup request, including provider-resolved defaults, so
	// its replay reaches the capture-owner guard rather than an intent conflict.
	cfg := defaultConfig()
	cfg.Provider, cfg.TargetOS, cfg.Network = "machine0", "linux", NetworkPublic
	cfg.Machine0.Size, cfg.Machine0.Region, cfg.Machine0.Image = "large", "eu", "ubuntu-24-04"
	cfg.ServerType, cfg.WorkRoot = cfg.Machine0.Size, cfg.Machine0.WorkRoot
	cfg.WindowsMode, cfg.SSHPort, cfg.SSHFallbackPorts = "", "22", nil
	fingerprint, err := FixedMachine0CreateIntentFingerprint(cfg, FixedMachine0CreateIntentRequest{Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := json.Marshal(map[string]any{"name": name, "size": cfg.Machine0.Size, "region": cfg.Machine0.Region, "image": cfg.Machine0.Image, "imageVersion": cfg.Machine0.ImageVersion, "createdAt": time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	claim := leaseClaim{
		LeaseID: captureFixtureLease, Revision: "fixture-generation-1", Slug: "proof-owned",
		Provider: FixedMachine0ClaimProvider, CloudID: captureFixtureSource, CloudImmutableID: captureFixtureSource, ProviderScope: "machine0:" + captureFixtureSource,
		RepoRoot: repo, ClaimedAt: "2026-01-01T00:00:00Z", LastUsedAt: "2026-01-01T00:00:00Z", TargetOS: "linux",
		Labels:            map[string]string{"machine0_name": name, "machine0_id": captureFixtureSource, "region": "eu", "lease": captureFixtureLease, "slug": "proof-owned"},
		FixedCreateIntent: &FixedCreateIntent{Version: 1, Fingerprint: fingerprint, ProviderScope: "machine0:name:" + name, Slug: "proof-owned", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), State: "acquired", Attempt: map[string]string{"machine0": string(attempt)}},
	}
	f.writeJSON(filepath.Join(root, "state", "crabbox", "claims", captureFixtureLease+".json"), claim)
	f.writeState(checkpointCaptureFixtureState{Machine: map[string]any{
		"id": captureFixtureSource, "name": name, "status": "RUNNING", "ip": "127.0.0.1",
		"size": "large", "vcpu": 2, "ram": 4, "disk": 80, "region": "eu", "image": "ubuntu-24-04", "defaultSSHUsername": "ubuntu", "distribution": "ubuntu",
	}})
	fifo := filepath.Join(root, "events.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Open(fifo, syscall.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	reader := os.NewFile(uintptr(fd), fifo)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			var event checkpointCaptureFixtureEvent
			if json.Unmarshal(scanner.Bytes(), &event) == nil {
				f.events <- event
			}
		}
	}()
	t.Cleanup(func() { _ = reader.Close(); <-stopped })
	return f
}

type checkpointCaptureProcess struct {
	cmd             *exec.Cmd
	owner           *pondMeshExecHandle
	ctx             context.Context
	output          checkpointCaptureOutput
	done            chan struct{}
	err             error
	cancel          context.CancelFunc
	killed          sync.Once
	groupMu         sync.Mutex
	groupStopped    bool
	groupTerminated bool
	nativeLifetime  *os.File
	nativeDirectory string
	nativeClose     sync.Once
}

type checkpointCaptureOutput struct {
	mu      sync.Mutex
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	marker  string
	matched chan struct{}
	once    sync.Once
}

type checkpointCaptureWriter struct {
	output *checkpointCaptureOutput
	stderr bool
}

func (w checkpointCaptureWriter) Write(p []byte) (int, error) {
	w.output.mu.Lock()
	defer w.output.mu.Unlock()
	buffer := &w.output.stdout
	if w.stderr {
		buffer = &w.output.stderr
	}
	n, err := buffer.Write(p)
	if w.stderr && w.output.marker != "" && strings.Contains(buffer.String(), w.output.marker) {
		w.output.once.Do(func() { close(w.output.matched) })
	}
	return n, err
}

func (p *checkpointCaptureProcess) waitMarker(t *testing.T) {
	t.Helper()
	select {
	case <-p.output.matched:
	case <-p.done:
		t.Fatalf("CLI exited before pending snapshot observation: %v\n%s", p.err, p.output.stderr.String())
	}
}

func (f *checkpointCaptureFixture) start(marker string, args ...string) *checkpointCaptureProcess {
	f.t.Helper()
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	p := &checkpointCaptureProcess{cmd: exec.CommandContext(ctx, f.binary, args...), ctx: ctx, done: make(chan struct{}), cancel: cancel}
	directory, err := os.MkdirTemp(f.root, "native-lifetime-")
	if err != nil {
		cancel()
		f.t.Fatal(err)
	}
	p.nativeDirectory = directory
	f.t.Cleanup(p.closeNativeHelpers)
	lifetime := filepath.Join(directory, "owner.fifo")
	if err := syscall.Mkfifo(lifetime, 0o600); err != nil {
		cancel()
		f.t.Fatal(err)
	}
	p.nativeLifetime, err = os.OpenFile(lifetime, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		cancel()
		f.t.Fatal(err)
	}
	p.owner = &pondMeshExecHandle{cmd: p.cmd, managed: true}
	p.output.marker, p.output.matched = marker, make(chan struct{})
	p.cmd.Dir, p.cmd.Env = f.repo, append(append([]string(nil), f.env...), "CRABBOX_CAPTURE_LIFETIME="+lifetime)
	// Cancel must only signal: Cmd.Wait receives the context watcher's result.
	p.cmd.Cancel = func() error { return p.signalGroup(syscall.SIGKILL) }
	// Own the pipes so Cmd.Wait can reap the leader before descendants close
	// inherited descriptors. Drain them only after terminating the owned groups.
	var readers, writers []*os.File
	defer func() {
		for _, file := range writers {
			_ = file.Close()
		}
	}()
	for range 2 {
		reader, writer, err := os.Pipe()
		if err != nil {
			for _, file := range readers {
				_ = file.Close()
			}
			cancel()
			f.t.Fatal(err)
		}
		readers, writers = append(readers, reader), append(writers, writer)
	}
	p.cmd.Stdout, p.cmd.Stderr = writers[0], writers[1]
	if err := p.owner.Start(); err != nil {
		for _, file := range readers {
			_ = file.Close()
		}
		cause := context.Cause(ctx)
		cancel()
		f.t.Fatalf("checkpoint Start after %s: %v cause_before_cancel=%v", time.Since(started), err, cause)
	}
	startElapsed := time.Since(started)
	f.t.Logf("checkpoint Start pid=%d pgid=%d elapsed=%s binary=%s args=%q", p.cmd.Process.Pid, p.owner.platform.anchor.Process.Pid, startElapsed, f.binary, args)
	drained := make(chan error, len(readers))
	for i, reader := range readers {
		go func() {
			_, err := io.Copy(checkpointCaptureWriter{output: &p.output, stderr: i == 1}, reader)
			_ = reader.Close()
			drained <- err
		}()
	}
	go func() {
		p.err = p.cmd.Wait()
		waitElapsed, cause := time.Since(started), context.Cause(ctx)
		// Seal signaling before reaping the anchor, so late cleanup can never
		// target a recycled PGID, even when the invocation leader exited first.
		f.signalGroups(p)
		cleanupErr := p.owner.finishPondMeshPlatform()
		var exitErr *exec.ExitError
		if p.groupTerminated && errors.As(cleanupErr, &exitErr) {
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGTERM {
				cleanupErr = nil // The contract deliberately sent SIGTERM to this group.
			}
		}
		p.err = errors.Join(p.err, cleanupErr)
		for range readers {
			p.err = errors.Join(p.err, <-drained)
		}
		f.t.Logf("checkpoint Wait pid=%d pgid=%d start=%s wait=%s drained=%s err=%v cause_before_cancel=%v boundary=%s", p.cmd.Process.Pid, p.owner.platform.anchor.Process.Pid, startElapsed, waitElapsed, time.Since(started), p.err, cause, f.lastBoundary(p))
		p.cancel()
		close(p.done)
	}()
	f.t.Cleanup(func() { f.kill(p) })
	return p
}

func (p *checkpointCaptureProcess) signalGroup(signal syscall.Signal) error {
	p.groupMu.Lock()
	defer p.groupMu.Unlock()
	if p.groupStopped {
		return os.ErrProcessDone
	}
	// The existing owner's anchor is unreaped until signaling is sealed.
	group := p.owner.platform.anchor.Process.Pid
	if group <= 1 || group == syscall.Getpgrp() {
		return fmt.Errorf("refusing unowned checkpoint group %d", group)
	}
	err := syscall.Kill(-group, signal)
	if signal == syscall.SIGKILL {
		p.groupStopped = true
	}
	if signal == syscall.SIGTERM && err == nil {
		p.groupTerminated = true
	}
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func (p *checkpointCaptureProcess) closeNativeHelpers() {
	p.nativeClose.Do(func() {
		if p.nativeLifetime != nil {
			_ = p.nativeLifetime.Close()
		}
		_ = os.RemoveAll(p.nativeDirectory)
	})
}

func (f *checkpointCaptureFixture) signalGroups(p *checkpointCaptureProcess) {
	// Native fixture helpers exit on their invocation's EOF. Historical event
	// PIDs may already be reaped/reused and never authorize an external signal.
	p.closeNativeHelpers()
	if err := p.signalGroup(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
		// Darwin reports EPERM for a group containing only the SIGTERM-killed
		// zombie anchor. Keep live-group failures and inspect outside Cancel.
		group := p.owner.platform.anchor.Process.Pid
		if runtime.GOOS == "darwin" && p.groupTerminated && errors.Is(err, syscall.EPERM) && !controllerProcessGroupAlive(group) {
			f.t.Logf("checkpoint group=%d has no live members after SIGTERM; reaping anchor", group)
		} else {
			f.t.Errorf("signal checkpoint group: %v", err)
		}
	}
}

func (f *checkpointCaptureFixture) lastBoundary(p *checkpointCaptureProcess) string {
	last := "no recorded native or Docker boundary"
	for _, event := range f.eventLog() {
		if event.Owner == p.cmd.Process.Pid {
			last = fmt.Sprintf("native %s pid=%d pgid=%d owner=%d", event.Kind, event.PID, event.PGID, event.Owner)
		}
	}
	if data, err := os.ReadFile(filepath.Join(f.root, "docker.json")); err == nil {
		var state captureContainerState
		if err := json.Unmarshal(data, &state); err != nil {
			return fmt.Sprintf("Docker state unreadable: %v", err)
		}
		if n := len(state.Calls); n > 0 {
			last = fmt.Sprintf("Docker last persisted call=%q cumulative_calls=%d", state.Calls[n-1], n)
		}
	}
	return last
}

func (f *checkpointCaptureFixture) kill(p *checkpointCaptureProcess) {
	p.killed.Do(func() {
		f.t.Logf("checkpoint cleanup pid=%d cause_before_cancel=%v", p.cmd.Process.Pid, context.Cause(p.ctx))
		f.signalGroups(p)
		p.cancel()
		<-p.done
	})
}

func (f *checkpointCaptureFixture) waitEvent(p *checkpointCaptureProcess, kind string) {
	f.t.Helper()
	for {
		select {
		case event := <-f.events:
			if event.Kind == kind {
				return
			}
		case <-p.done:
			f.t.Fatalf("CLI exited before %s: %v\n%s", kind, p.err, p.output.stderr.String())
		}
	}
}

type checkpointCaptureResult struct {
	stdout []byte
	stderr []byte
	err    error
}

func (f *checkpointCaptureFixture) run(args ...string) checkpointCaptureResult {
	p := f.start("", args...)
	<-p.done
	return checkpointCaptureResult{append([]byte(nil), p.output.stdout.Bytes()...), append([]byte(nil), p.output.stderr.Bytes()...), p.err}
}

func (f *checkpointCaptureFixture) retireArgs() []string {
	return []string{"checkpoint", "create", "--provider", "machine0", "--id", captureFixtureLease, "--checkpoint-id", captureFixtureCheckpoint, "--retire-source", "--mode", "native", "--wait=false", "--json"}
}

func (f *checkpointCaptureFixture) finishRetirement() {
	f.t.Helper()
	for attempt := 0; attempt < 4; attempt++ {
		result := f.run(f.retireArgs()...)
		if result.err != nil {
			f.t.Fatalf("same-operation replay failed: %v\n%s\n%s", result.err, result.stdout, result.stderr)
		}
		var record checkpointRecord
		if err := json.Unmarshal(result.stdout, &record); err != nil {
			f.t.Fatalf("replay JSON: %v\n%s", err, result.stdout)
		}
		if record.ID != captureFixtureCheckpoint || record.Capture == nil || record.Capture.SourceID != captureFixtureSource {
			f.t.Fatalf("replay changed durable identity: %+v", record)
		}
		if record.Capture.Phase == "retired" {
			if record.Native.ImageID != "fixture-image-immutable-id@v1" || f.state().Machine != nil {
				f.t.Fatalf("retired without bound image and proved source absence: %+v", record)
			}
			return
		}
	}
	f.t.Fatalf("ready provider did not retire within bounded replay: %+v", f.record(captureFixtureCheckpoint))
}

func (f *checkpointCaptureFixture) requirePending() {
	f.t.Helper()
	result := f.run(f.retireArgs()...)
	if result.err != nil {
		f.t.Fatalf("capture did not return bounded pending handle: %v\n%s", result.err, result.stderr)
	}
	record := f.record(captureFixtureCheckpoint)
	if record.Capture == nil || record.Capture.Phase != "pending" || record.Native.ImageID == "" {
		f.t.Fatalf("capture did not persist observed pending image: %+v", record)
	}
}

func (f *checkpointCaptureFixture) scrub() {
	f.t.Helper()
	scrubber := *f
	scrubber.binary = filepath.Join(f.root, "bin", "ssh")
	p := scrubber.start("", "fixture-source", "fixture-scrub")
	<-p.done
	if p.err != nil {
		f.t.Fatalf("fixture scrub: %v\n%s", p.err, p.output.stderr.String())
	}
}

func (f *checkpointCaptureFixture) state() (value checkpointCaptureFixtureState) {
	f.readJSON(filepath.Join(f.root, "provider.json"), &value)
	return value
}

func (f *checkpointCaptureFixture) writeState(value checkpointCaptureFixtureState) {
	f.writeJSON(filepath.Join(f.root, "provider.json"), value)
}

func (f *checkpointCaptureFixture) claim() (value leaseClaim) {
	f.readJSON(filepath.Join(f.root, "state", "crabbox", "claims", captureFixtureLease+".json"), &value)
	return value
}

func (f *checkpointCaptureFixture) record(id string) (value checkpointRecord) {
	f.readJSON(filepath.Join(f.root, "state", "crabbox", "checkpoints", id, checkpointMetaFile), &value)
	return value
}

func (f *checkpointCaptureFixture) readJSON(path string, value any) {
	f.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		f.t.Fatal(err)
	}
}

func (f *checkpointCaptureFixture) writeJSON(path string, value any) {
	f.t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *checkpointCaptureFixture) eventLog() []checkpointCaptureFixtureEvent {
	data, _ := os.ReadFile(filepath.Join(f.root, "events.jsonl"))
	var events []checkpointCaptureFixtureEvent
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var event checkpointCaptureFixtureEvent
		if json.Unmarshal(line, &event) == nil {
			events = append(events, event)
		}
	}
	return events
}

func (f *checkpointCaptureFixture) requireOrder(kinds ...string) {
	f.t.Helper()
	remaining := kinds
	for _, event := range f.eventLog() {
		if len(remaining) > 0 && event.Kind == remaining[0] {
			remaining = remaining[1:]
		}
	}
	if len(remaining) != 0 {
		f.t.Fatalf("missing event order %v in %+v", kinds, f.eventLog())
	}
}
