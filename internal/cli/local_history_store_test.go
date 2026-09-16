package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func localHistoryTestRecord(n int) localHistoryRecord {
	return localHistoryRecord{Version: 1, RecordingState: "active", ID: fmt.Sprintf("run_%032x", n), Source: "local", Provider: "synthetic", Phase: "admitted", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Stdout: localHistoryStream{Availability: "retained"}, Stderr: localHistoryStream{Availability: "retained"}, ResultsState: "no-results"}
}
func localHistoryTestHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	return filepath.Join(root, "crabbox", "history")
}

func TestLocalHistoryStorePrivateCreation(t *testing.T) {
	path := localHistoryTestHome(t)
	record := localHistoryTestRecord(900)
	writer, err := beginLocalHistory(record)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.Commit(record, runLogSnapshot{Log: "ordinary output\n"}, &TestResultSummary{Format: "junit", Tests: 1}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(path, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		file, err := os.Open(name)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := localHistoryPrivate(file, entry.IsDir()); err != nil {
			return fmt.Errorf("created %s: %w", filepath.Base(name), err)
		}
		if runtime.GOOS != "windows" {
			info, err := file.Stat()
			if err != nil {
				return err
			}
			want := os.FileMode(0o600)
			if entry.IsDir() {
				want = 0o700
			}
			if info.Mode().Perm() != want {
				return fmt.Errorf("created mode %o, want %o", info.Mode().Perm(), want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLocalHistoryStoreRejectsUnixAliasesAndPublicMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode and link-count contract; Windows creation ACLs run separately")
	}
	for _, kind := range []string{"symlink", "hardlink", "public-mode"} {
		t.Run(kind, func(t *testing.T) {
			path := localHistoryTestHome(t)
			record := localHistoryTestRecord(901)
			writer, err := beginLocalHistory(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(path, "runs", record.ID, "record.json")
			original, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			ownedCopy := filepath.Join(t.TempDir(), "record-copy.json")
			switch kind {
			case "symlink":
				err = os.Rename(name, ownedCopy)
				if err == nil {
					err = os.Symlink(ownedCopy, name)
				}
			case "hardlink":
				err = os.Link(name, ownedCopy)
			case "public-mode":
				err = os.Chmod(name, 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := readLocalHistory(record.ID); err == nil {
				t.Fatal("expected private record admission rejection")
			}
			data, err := os.ReadFile(name)
			if err != nil || string(data) != string(original) {
				t.Fatalf("fixture modified: %v", err)
			}
		})
	}
}
func TestLocalHistoryStoreLifecycle(t *testing.T) {
	localHistoryTestHome(t)
	available, err := localHistoryAvailable()
	if err != nil || available {
		t.Fatalf("initial availability %v %v", available, err)
	}
	records, err := listLocalHistory(localHistoryFilter{})
	if err != nil || len(records) != 0 {
		t.Fatalf("initial list %v %v", records, err)
	}
	record := localHistoryTestRecord(1)
	writer, err := beginLocalHistory(record)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	records, err = listLocalHistory(localHistoryFilter{})
	if err != nil || len(records) != 1 || records[0].RecordingState != "active" {
		t.Fatalf("active list %+v %v", records, err)
	}
	if err := deleteLocalHistory(record.ID); !errors.Is(err, errLocalHistoryBusy) {
		t.Fatalf("active delete %v", err)
	}
	record.Phase = "command"
	if err := writer.Checkpoint(record); err != nil {
		t.Fatal(err)
	}
	summary := &TestResultSummary{Format: "junit", Tests: 7, Failures: 2, Failed: []TestFailure{{Name: "one", Message: "failure"}}}
	record.ResultsState = "collected"
	zero := 0
	record.ExitCode = &zero
	if err := writer.Commit(record, runLogSnapshot{Log: "out\nerr\n"}, summary); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	got, log, results, err := readLocalHistory(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RecordingState != "terminal" || log != "out\nerr\n" || results.Tests != 7 || results.Failures != 2 || got.LogSHA256 == "" || got.ResultsSHA256 == "" {
		t.Fatalf("terminal %+v %q %+v", got, log, results)
	}
	duplicate, err := beginLocalHistory(record)
	if duplicate != nil {
		duplicate.Close()
		t.Fatal("duplicate run ID admitted")
	}
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("duplicate admission=%v, want os.ErrExist", err)
	}
	if retained, text, _, err := readLocalHistory(record.ID); err != nil || retained.RecordingState != "terminal" || text != log {
		t.Fatalf("duplicate admission changed record=%+v log=%q err=%v", retained, text, err)
	}
	if err := deleteLocalHistory(record.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readLocalHistory(record.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted read %v", err)
	}
}
func TestLocalHistoryStoreAbandonedAndPartialAreNotTerminal(t *testing.T) {
	path := localHistoryTestHome(t)
	record := localHistoryTestRecord(2)
	writer, err := beginLocalHistory(record)
	if err != nil {
		t.Fatal(err)
	}
	// Closing without committing models the lock release on process death.
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "runs", record.ID, "log.txt"), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, log, summary, err := readLocalHistory(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RecordingState != "incomplete" || got.Stdout.Availability != "unavailable" || log != "" || summary != nil {
		t.Fatalf("abandoned %+v %q %+v", got, log, summary)
	}
}

func TestLocalHistoryStoreInterruptedInitializationKeepsHealthyRows(t *testing.T) {
	for _, withLock := range []bool{false, true} {
		t.Run(fmt.Sprintf("lock=%t", withLock), func(t *testing.T) {
			localHistoryTestHome(t)
			healthy := localHistoryTestRecord(910)
			writer, err := beginLocalHistory(healthy)
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.Commit(healthy, runLogSnapshot{Log: "healthy\n"}, nil); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			root, err := localHistoryRoot(false)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			runs, err := localHistoryChild(root, "runs")
			if err != nil {
				t.Fatal(err)
			}
			defer runs.Close()
			id := localHistoryTestRecord(911).ID
			if err := localHistoryMkdir(runs, id); err != nil {
				t.Fatal(err)
			}
			if withLock {
				dir, err := localHistoryChild(runs, id)
				if err != nil {
					t.Fatal(err)
				}
				lock, err := localHistoryLock(dir, true, false)
				if err != nil {
					dir.Close()
					t.Fatal(err)
				}
				err = localHistoryUnlock(lock)
				dir.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			rows, err := listLocalHistory(localHistoryFilter{})
			if err != nil || len(rows) != 2 || rows[0].ID != healthy.ID {
				t.Fatalf("rows=%+v err=%v", rows, err)
			}
			incomplete, log, results, err := readLocalHistory(id)
			if err != nil || incomplete.RecordingState != "incomplete" || incomplete.Provider != "" || incomplete.CommandDisplay != "" || incomplete.ExitCode != nil || incomplete.StartedAt != "" || incomplete.Stdout.Availability != "unavailable" || incomplete.Stderr.Availability != "unavailable" || incomplete.ResultsState != "unavailable" || log != "" || results != nil {
				t.Fatalf("incomplete=%+v log=%q results=%+v err=%v", incomplete, log, results, err)
			}
			filtered, err := listLocalHistory(localHistoryFilter{State: "incomplete"})
			if err != nil || len(filtered) != 1 || filtered[0].ID != id {
				t.Fatalf("incomplete filter=%+v err=%v", filtered, err)
			}
			if _, log, _, err := readLocalHistory(healthy.ID); err != nil || log != "healthy\n" {
				t.Fatalf("healthy log=%q err=%v", log, err)
			}
		})
	}
}

func TestLocalHistoryStoreMissingLockWithPublishedMetadataRemainsError(t *testing.T) {
	path := localHistoryTestHome(t)
	record := localHistoryTestRecord(912)
	writer, err := beginLocalHistory(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(path, "runs", record.ID, "write.lock")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readLocalHistory(record.ID); err == nil {
		t.Fatal("published metadata without a lock was accepted")
	}
	if _, err := listLocalHistory(localHistoryFilter{}); err == nil {
		t.Fatal("listing accepted published metadata without a lock")
	}
}
func TestLocalHistoryStoreCommitFailureLeavesIncomplete(t *testing.T) {
	path := localHistoryTestHome(t)
	record := localHistoryTestRecord(3)
	writer, err := beginLocalHistory(record)
	if err != nil {
		t.Fatal(err)
	}
	// A known leftover temporary is never overwritten by a later publication.
	if err := os.WriteFile(filepath.Join(path, "runs", record.ID, ".log.txt.tmp"), []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(record, runLogSnapshot{Log: "new"}, nil); err == nil {
		t.Fatal("wanted persistence failure")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	got, log, _, err := readLocalHistory(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RecordingState != "incomplete" || log != "" {
		t.Fatalf("failed commit %+v %q", got, log)
	}
}
func TestLocalHistoryStoreRetentionIncludesIncomplete(t *testing.T) {
	path := localHistoryTestHome(t)
	for i := 0; i < localHistoryInactiveLimit+2; i++ {
		record := localHistoryTestRecord(i + 10)
		writer, err := beginLocalHistory(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	records, err := listLocalHistory(localHistoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != localHistoryInactiveLimit {
		t.Fatalf("retained %d", len(records))
	}
	if _, err := os.Stat(filepath.Join(path, "runs", localHistoryTestRecord(10).ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest retained: %v", err)
	}
	old := records[0]
	old.UpdatedAt = time.Now().Add(-31 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "runs", old.ID, "record.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := pruneLocalHistory()
	if err != nil || removed != 1 {
		t.Fatalf("age prune %d %v", removed, err)
	}
}
func TestLocalHistoryStoreActiveReservationAndPrune(t *testing.T) {
	localHistoryTestHome(t)
	var writers []*localHistoryWriter
	defer func() {
		for _, writer := range writers {
			writer.Close()
		}
	}()
	for i := 0; i < 25; i++ {
		writer, err := beginLocalHistory(localHistoryTestRecord(i + 200))
		if err != nil {
			t.Fatalf("admit %d: %v", i, err)
		}
		writers = append(writers, writer)
	}
	if writer, err := beginLocalHistory(localHistoryTestRecord(300)); err == nil {
		writer.Close()
		t.Fatal("active reservations exceeded store capacity")
	}
	if removed, err := pruneLocalHistory(); err != nil || removed != 0 {
		t.Fatalf("active prune %d %v", removed, err)
	}
	if err := writers[0].Close(); err != nil {
		t.Fatal(err)
	}
	writer, err := beginLocalHistory(localHistoryTestRecord(301))
	if err != nil {
		t.Fatal(err)
	}
	writers = append(writers, writer)
}
func TestLocalHistoryStoreBoundedProjection(t *testing.T) {
	record := localHistoryTestRecord(400)
	record.UpdatedAt = record.StartedAt
	record.CommandDisplay = strings.Repeat("🙂", 10000)
	record.Diagnostic = strings.Repeat("\x00", 30000)
	bounded, data, err := localHistoryRecordBytes(record)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > localHistoryMetadataLimit || !bounded.CommandDisplayTruncated || !bounded.DiagnosticTruncated || !utf8.ValidString(bounded.CommandDisplay) {
		t.Fatalf("metadata bounds %+v %d", bounded, len(data))
	}
	input := &TestResultSummary{Format: "junit", Tests: 100000, Failures: 9999, TimeSeconds: 100.25}
	for i := 0; i < 500; i++ {
		input.Files = append(input.Files, fmt.Sprintf("file-%d.xml", i))
		input.Failed = append(input.Failed, TestFailure{Name: fmt.Sprintf("test-%d", i), Message: strings.Repeat("\x00", 9000)})
	}
	resultData, err := localHistoryResultBytes(input, &record)
	if err != nil {
		t.Fatal(err)
	}
	var got TestResultSummary
	if err := json.Unmarshal(resultData, &got); err != nil {
		t.Fatal(err)
	}
	if len(resultData) > localHistoryResultsLimit || got.Tests != input.Tests || got.Failures != input.Failures || got.TimeSeconds != input.TimeSeconds || !record.ResultsDetailsTruncated || record.ResultsOmittedFailures != len(input.Failed)-len(got.Failed) || record.ResultsClippedFields == 0 {
		t.Fatalf("projection bytes=%d totals=%+v markers=%+v", len(resultData), got, record)
	}
	if input.Failed[0].Message != strings.Repeat("\x00", 9000) {
		t.Fatal("mutated parser output")
	}
}
func TestLocalHistoryStoreIntegrityAndUnmanagedEntries(t *testing.T) {
	path := localHistoryTestHome(t)
	record := localHistoryTestRecord(500)
	writer, err := beginLocalHistory(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(record, runLogSnapshot{Log: "original"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "runs", record.ID, "log.txt"), []byte("modified"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readLocalHistory(record.ID); err == nil {
		t.Fatal("accepted corrupt payload")
	}
	if err := os.WriteFile(filepath.Join(path, "unmanaged"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := beginLocalHistory(localHistoryTestRecord(501)); err == nil {
		t.Fatal("admitted unmanaged footprint")
	}
	if data, err := os.ReadFile(filepath.Join(path, "unmanaged")); err != nil || string(data) != "preserve" {
		t.Fatalf("unmanaged changed %q %v", data, err)
	}
}
func TestLocalHistoryStoreMissingMetadataPrunesAsIncomplete(t *testing.T) {
	path := localHistoryTestHome(t)
	writer, err := beginLocalHistory(localHistoryTestRecord(600))
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(path, "runs", localHistoryTestRecord(601).ID)
	runs, err := os.OpenRoot(filepath.Join(path, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	defer runs.Close()
	if err := localHistoryMkdir(runs, localHistoryTestRecord(601).ID); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := pruneLocalHistory()
	if err != nil || removed != 1 {
		t.Fatalf("orphan prune %d %v", removed, err)
	}
}

func TestLocalHistoryStoreConcurrentCheckpointPrune(t *testing.T) {
	localHistoryTestHome(t)
	record := localHistoryTestRecord(700)
	writer, err := beginLocalHistory(record)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	wg.Go(func() {
		for i := 0; i < 40; i++ {
			record.Phase = "command"
			if err := writer.Checkpoint(record); err != nil {
				failures <- err
				return
			}
		}
	})
	wg.Go(func() {
		for i := 0; i < 40; i++ {
			removed, err := pruneLocalHistory()
			if err != nil {
				failures <- err
				return
			}
			if removed != 0 {
				failures <- fmt.Errorf("pruned active writer")
				return
			}
		}
	})
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}
func TestLocalHistoryStoreProcessLockHelper(t *testing.T) {
	if os.Getenv("CRABBOX_LOCAL_HISTORY_LOCK_HELPER") != "1" {
		return
	}
	t.Setenv("XDG_STATE_HOME", os.Getenv("CRABBOX_LOCAL_HISTORY_LOCK_ROOT"))
	writer, err := beginLocalHistory(localHistoryTestRecord(800))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, "locked")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	runtime.KeepAlive(writer) // No Close: the process releases the active OS lock.
}
func TestLocalHistoryStoreProcessExitReleasesLock(t *testing.T) {
	localHistoryTestHome(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestLocalHistoryStoreProcessLockHelper$")
	cmd.Env = append(os.Environ(), "CRABBOX_LOCAL_HISTORY_LOCK_HELPER=1", "CRABBOX_LOCAL_HISTORY_LOCK_ROOT="+os.Getenv("XDG_STATE_HOME"))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "locked\n" {
			t.Fatalf("child readiness %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child readiness timed out")
	}
	id := localHistoryTestRecord(800).ID
	if err := deleteLocalHistory(id); !errors.Is(err, errLocalHistoryBusy) {
		t.Fatalf("child lock ignored: %v", err)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	finished = true
	record, _, _, err := readLocalHistory(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.RecordingState != "incomplete" {
		t.Fatalf("abandoned state %s", record.RecordingState)
	}
	if err := deleteLocalHistory(id); err != nil {
		t.Fatal(err)
	}
}
func TestLocalHistoryStoreDeleteCorruptMetadata(t *testing.T) {
	path := localHistoryTestHome(t)
	record := localHistoryTestRecord(900)
	writer, err := beginLocalHistory(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "runs", record.ID, "record.json"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listLocalHistory(localHistoryFilter{}); err == nil {
		t.Fatal("corruption not reported")
	}
	if err := deleteLocalHistory(record.ID); err != nil {
		t.Fatal(err)
	}
}

func TestLocalHistoryStoreStateFilters(t *testing.T) {
	localHistoryTestHome(t)
	for i, status := range []RunStatus{RunStatusSucceeded, RunStatusFailed, RunStatusCanceled, RunStatusTimedOut} {
		record := localHistoryTestRecord(950 + i)
		record.RunStatus = status
		writer, err := beginLocalHistory(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.Commit(record, runLogSnapshot{}, nil); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, state := range []string{"succeeded", "failed", "canceled", "timed-out", "terminal", "active", "incomplete"} {
		records, err := listLocalHistory(localHistoryFilter{State: state})
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if state == "terminal" {
			want = 4
		}
		if state == "active" || state == "incomplete" {
			want = 0
		}
		if len(records) != want {
			t.Fatalf("state %s got %d want %d", state, len(records), want)
		}
	}
	if _, err := listLocalHistory(localHistoryFilter{State: "bogus"}); err == nil {
		t.Fatal("invalid state accepted")
	}
}

func TestLocalHistoryStoreRealRunLogSnapshot(t *testing.T) {
	localHistoryTestHome(t)
	var buffer runLogBuffer
	if _, err := buffer.Write([]byte(strings.Repeat("x", maxRunLogBytes+100))); err != nil {
		t.Fatal(err)
	}
	if _, err := (capturedRunLogWriter{buffer: &buffer}).Write([]byte("capture-only-marker")); err != nil {
		t.Fatal(err)
	}
	if _, err := buffer.Write([]byte{0xff}); err != nil {
		t.Fatal(err)
	}
	snapshot := buffer.Snapshot()
	record := localHistoryTestRecord(1000)
	writer, err := beginLocalHistory(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(record, snapshot, nil); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	got, log, _, err := readLocalHistory(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if log != snapshot.Log || got.LogFullSHA256 != snapshot.FullSHA256 || !got.LogTruncated || len(log) > maxRunLogBytes || !utf8.ValidString(log) || strings.Contains(log, "capture-only-marker") {
		t.Fatalf("actual bounded snapshot changed: logBytes=%d rawDigest=%s truncated=%v", len(log), got.LogFullSHA256, got.LogTruncated)
	}
}

func TestLocalHistoryStoreClipMalformedPrefix(t *testing.T) {
	cases := []struct {
		name, input, want string
		limit             int
		changed           bool
	}{
		{"long ASCII after invalid byte", "\xff" + strings.Repeat("a", 100), "�" + strings.Repeat("a", 29), 32, true},
		{"long Unicode after invalid byte", "\xff" + strings.Repeat("界", 100), "�" + strings.Repeat("界", 9), 32, true},
		{"short malformed text", "a\xffz", "a�z", 32, true},
		{"valid crossing rune", "🙂rest", "", 2, true},
		{"valid exact boundary", "🙂rest", "🙂r", 5, true},
		{"unchanged", "hello", "hello", 32, false},
		{"zero budget", "x", "", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := localHistoryClip(tc.input, tc.limit)
			if got != tc.want || changed != tc.changed || len(got) > tc.limit || !utf8.ValidString(got) {
				t.Fatalf("got %q changed=%v, want %q changed=%v", got, changed, tc.want, tc.changed)
			}
		})
	}
}
func TestLocalHistoryStoreCoordinatorReferenceValidation(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, reference := range []string{"", digest, "sha256:" + digest} {
		record := localHistoryTestRecord(1100)
		record.UpdatedAt = record.StartedAt
		record.CoordinatorReference = reference
		got, _, err := localHistoryRecordBytes(record)
		if err != nil || got.CoordinatorReference != reference {
			t.Fatalf("valid reference rejected: %v", err)
		}
	}
	for _, reference := range []string{"sha256:", "https://example.invalid/", strings.Repeat("g", 64), strings.Repeat("a", localHistoryMetadataLimit+1)} {
		record := localHistoryTestRecord(1100)
		record.UpdatedAt = record.StartedAt
		record.CoordinatorReference = reference
		if _, _, err := localHistoryRecordBytes(record); err == nil {
			t.Fatal("invalid reference accepted")
		}
	}
}
func TestLocalHistoryStoreEquivalentTimestampTie(t *testing.T) {
	localHistoryTestHome(t)
	for i, stamp := range []string{"2026-01-01T01:00:00+01:00", "2026-01-01T00:00:00.000Z"} {
		record := localHistoryTestRecord(1201 - i)
		record.StartedAt = stamp
		writer, err := beginLocalHistory(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	records, err := listLocalHistory(localHistoryFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != localHistoryTestRecord(1200).ID {
		t.Fatalf("timestamp tie %+v", records)
	}
}
