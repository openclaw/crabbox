package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

type pendingCleanupTimingWriter struct {
	bytes.Buffer
	writes   int
	rejectAt int
}

func (w *pendingCleanupTimingWriter) WriteTimingReport(report TimingReport) error {
	w.writes++
	if w.writes == w.rejectAt {
		return errors.New("late timing sink unavailable")
	}
	return encodeTimingJSON(&w.Buffer, report)
}

func TestRunCommandPendingFinishCleanupDiagnostic(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("fault injection requires Unix directory write permissions")
	}
	for _, tc := range []struct {
		name           string
		timingJSON     bool
		cleanupFault   string
		rejectTimingAt int
	}{
		{name: "text/normal"},
		{name: "text/remove", cleanupFault: "remove"},
		{name: "text/sync", cleanupFault: "sync"},
		{name: "timing/normal", timingJSON: true},
		{name: "timing/remove", timingJSON: true, cleanupFault: "remove"},
		{name: "timing/sync", timingJSON: true, cleanupFault: "sync"},
		{name: "timing/remove/late-sink-failure", timingJSON: true, cleanupFault: "remove", rejectTimingAt: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			timingJSON, failCleanup := tc.timingJSON, tc.cleanupFault != ""
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			sshPort := startTCPReadinessFixture(t)
			installWorkspaceOwnerAwareSSH(t, filepath.Join(dir, "ssh"), "#!/bin/sh\nexit 0\n")
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
			const leaseID = "cbx_cleanup_failure"
			lease := CoordinatorLease{ID: leaseID, Slug: "cleanup-failure", Provider: "run-ready-pool-preflight-test", Owner: "alice@example.com", Org: "example-org", Class: "standard", ServerType: "test", Host: "127.0.0.1", SSHUser: "crabbox", SSHPort: sshPort, WorkRoot: "/work/crabbox", State: "active"}
			var mu sync.Mutex
			var pendingPath string
			var original, posted []byte
			var receipt terminalRunReceipt
			var posts, readbacks int
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				runID := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/v1/runs/"), "/", 2)[0]
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/control":
					http.NotFound(w, r)
				case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+leaseID,
					r.Method == http.MethodPost && r.URL.Path == "/v1/leases/"+leaseID+"/heartbeat":
					_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
				case r.Method == http.MethodPut && r.URL.Path == "/v1/runs/"+runID:
					var body struct {
						Command []string `json:"command"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						http.Error(w, "decode", 400)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"run": CoordinatorRun{ID: runID, LeaseID: leaseID, Owner: lease.Owner, Org: lease.Org, Provider: lease.Provider, State: "running", Phase: "starting", Command: body.Command}})
				case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/"+runID+"/events":
					_ = json.NewEncoder(w).Encode(map[string]any{"event": CoordinatorRunEvent{RunID: runID, Seq: 1, Type: "run.event"}})
				case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/"+runID+"/finish":
					posts++
					var err error
					posted, err = io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						http.Error(w, "read", 400)
						return
					}
					pendingPath, err = pendingRunFinishPath(pendingRunFinish{Coordinator: server.URL, RunID: runID})
					if err != nil {
						t.Error(err)
						http.Error(w, "path", 500)
						return
					}
					original, err = os.ReadFile(pendingPath)
					if err != nil {
						t.Error(err)
						http.Error(w, "pending", 500)
						return
					}
					var pending pendingRunFinish
					if err := decodePendingFinishJSON(original, &pending); err != nil {
						t.Error(err)
						http.Error(w, "decode", 400)
						return
					}
					_, receipt, err = validatePendingRunFinish(pending)
					if err != nil {
						t.Error(err)
						http.Error(w, "validate", 400)
						return
					}
					if !bytes.Equal(posted, pending.Body) {
						t.Error("POST changed retained original bytes")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"run": CoordinatorRun{ID: runID, State: "passed", ExitCode: intPointer(0)}})
				case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/"+runID+"/receipt":
					readbacks++
					if pendingPath == "" {
						t.Error("receipt read before original retention")
						http.NotFound(w, r)
						return
					}
					if failCleanup {
						mode := os.FileMode(0o500) // Removal fails, preserving the original file.
						if tc.cleanupFault == "sync" {
							mode = 0o300
						} // Removal succeeds; opening the directory for fsync fails.
						if err := os.Chmod(filepath.Dir(pendingPath), mode); err != nil {
							t.Error(err)
							http.Error(w, "chmod", 500)
							return
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"receipt": receipt})
				default:
					t.Errorf("unexpected fixture request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			t.Cleanup(func() {
				if pendingPath != "" {
					if err := os.Chmod(filepath.Dir(pendingPath), 0o700); err != nil {
						t.Error(err)
					}
				}
			})
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "test-token")
			timingStore := filepath.Join(dir, "timings.jsonl")
			args := []string{"--provider", lease.Provider, "--id", leaseID, "--no-sync", "--stop-after", "never", "--timing-record", timingStore}
			if timingJSON {
				args = append(args, "--timing-json")
			}
			args = append(args, "--", "true")
			var stdout bytes.Buffer
			stderr := pendingCleanupTimingWriter{rejectAt: tc.rejectTimingAt}
			outcome := &shardRunOutcome{}
			err := (App{Stdout: &stdout, Stderr: &stderr, runOutcome: outcome}).runCommand(context.Background(), args)
			if tc.rejectTimingAt != 0 {
				var exitErr ExitError
				if !AsExitError(err, &exitErr) || exitErr.Code != 7 || !strings.Contains(exitErr.Message, "late timing sink unavailable") {
					t.Fatalf("late sink error=%v, want exit 7", err)
				}
			} else if err != nil {
				t.Fatalf("run error=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if posts != 1 || readbacks != 1 {
				t.Fatalf("POSTs=%d readbacks=%d, want one of each", posts, readbacks)
			}
			if receipt.ExitCode != 0 || !outcome.Recorded || outcome.ExitCode != 0 || outcome.RunID != receipt.RunID {
				t.Fatalf("committed result changed: receipt=%+v outcome=%+v", receipt, outcome)
			}
			if err := verifyTerminalRunReceiptSignature(receipt); err != nil {
				t.Fatal(err)
			}
			after, readErr := os.ReadFile(pendingPath)
			if tc.cleanupFault == "remove" {
				if readErr != nil || !bytes.Equal(original, after) {
					t.Fatalf("removal failure lost original evidence: %v", readErr)
				}
				var pending pendingRunFinish
				if err := decodePendingFinishJSON(after, &pending); err != nil {
					t.Fatal(err)
				}
				_, retainedReceipt, err := validatePendingRunFinish(pending)
				if err != nil || retainedReceipt != receipt || !bytes.Equal(pending.Body, posted) {
					t.Fatalf("original signed finish changed: %v", err)
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatalf("normal cleanup left pending file: %v", readErr)
			}
			diagnostic := "terminal committed; pending finish cleanup failed:"
			if strings.Contains(stderr.String(), diagnostic) != failCleanup {
				t.Fatalf("cleanup diagnostic mismatch:\n%s", stderr.String())
			}
			records, recordErr := readBenchmarkTimingRecords(timingStore)
			if recordErr != nil || len(records) != 1 || records[0].Timing.ExitCode != 0 {
				t.Fatalf("benchmark observation changed/duplicated: records=%v error=%v", records, recordErr)
			}
			if timingJSON {
				wantWrites := 1
				if failCleanup {
					wantWrites = 2
				}
				if stderr.writes != wantWrites {
					t.Fatalf("timing writes=%d want=%d", stderr.writes, wantWrites)
				}
				lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
				var timingLines []string
				for _, line := range lines {
					var candidate TimingReport
					if json.Unmarshal([]byte(line), &candidate) == nil && candidate.Provider == lease.Provider {
						timingLines = append(timingLines, line)
					}
				}
				for _, line := range timingLines {
					if line != timingLines[0] {
						t.Fatal("late diagnostic changed the frozen timing report")
					}
				}
				if tc.rejectTimingAt == 0 {
					var report TimingReport
					if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
						t.Fatalf("last stderr line is not timing JSON: %v\n%s", err, stderr.String())
					}
					if report.ExitCode != 0 || report.RunID != receipt.RunID {
						t.Fatalf("final timing changed committed result: %+v", report)
					}
				}
			} else if stderr.writes != 0 {
				t.Fatalf("text mode wrote %d timing records", stderr.writes)
			}
			t.Logf("original signature verified, exact pending/body preserved=%t, committed exit=0, POSTs=%d readbacks=%d, timing=%t", tc.cleanupFault == "remove", posts, readbacks, timingJSON)
		})
	}
}
