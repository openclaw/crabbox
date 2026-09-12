package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunObservationDelegatedAppRoundTrip(t *testing.T) {
	clearConfigEnv(t)
	isolateRunTestUserDirs(t, t.TempDir())
	runModuleRuntimeTestRequests = nil
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader("export default {}\n")}).runCommand(context.Background(), []string{
		"--record-local", "--provider", "module-runtime-test", "--script-stdin",
	})
	if err != nil {
		t.Fatalf("run=%v stderr=%q", err, stderr.String())
	}
	if len(runModuleRuntimeTestRequests) != 1 {
		t.Fatalf("requests=%d", len(runModuleRuntimeTestRequests))
	}
	record, log, _, err := readLocalHistory(runModuleRuntimeTestRequests[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.RecordingState != "terminal" || record.LeaseID != "mod_test" || record.Slug != "module-runtime-test" || record.ExitCode == nil || *record.ExitCode != 0 || record.CaptureScope != "provider-run" {
		t.Fatalf("record=%+v", record)
	}
	if log != "fixture command stdout\nfixture command stderr\n" || !strings.Contains(stderr.String(), "fixture provider planning") {
		t.Fatalf("log=%q terminal stderr=%q", log, stderr.String())
	}
}

func TestRunObservationDirectAppWithoutTimingOutput(t *testing.T) {
	setupLocalContainerRunSessionTest(t, "")
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(t.Context(), []string{
		"--record-local", "--provider", "local-container", "--keep", "--no-sync", "--no-hydrate", "--", "true",
	})
	if err != nil {
		t.Fatalf("run=%v stderr=%q", err, stderr.String())
	}
	records, err := listLocalHistory(localHistoryFilter{})
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	record := records[0]
	if record.LeaseID != localContainerRunSessionTestLeaseID || record.Slug != "session-slug" || record.RecordingState != "terminal" || record.ExitCode == nil || *record.ExitCode != 0 || record.TotalMs <= 0 || record.CaptureScope != "workload" {
		t.Fatalf("record=%+v", record)
	}
	if !strings.Contains(stderr.String(), "run="+record.ID) || strings.Contains(stderr.String(), `"commandMs"`) {
		t.Fatalf("run identity or timing output changed: %q", stderr.String())
	}
}

func TestRunObservationDisabledHasNoLedger(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	o, err := beginRunObservation(Config{}, "run_00000000000000000000000000000001", []string{"true"}, io.Discard)
	if err != nil || o != nil {
		t.Fatalf("disabled=%v %v", o, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("disabled created state: %v %v", entries, err)
	}
}

func TestRunObservationCapturesOnlyCommandWriters(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var terminalOut, terminalErr bytes.Buffer
	id := "run_00000000000000000000000000000002"
	o, err := beginRunObservation(Config{RecordLocal: true, Provider: "fixture", TargetOS: "linux"}, id, []string{"printf", "sample"}, &terminalErr)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(&terminalErr, "unrelated provider hint\n")
	o.Phase(RunPhaseCommand)
	out, stderr := o.CommandWriters(&terminalOut, &terminalErr, RunOutputWorkload)
	io.WriteString(out, "stdout marker\n")
	io.WriteString(stderr, "stderr marker\n")
	o.finish(timingReport{Provider: "fixture", LeaseID: "lease-fixture", Slug: "fixture", RunStatus: RunStatusSucceeded, CommandMs: 2, TotalMs: 3}, true, nil, nil)
	record, log, _, err := readLocalHistory(id)
	if err != nil {
		t.Fatal(err)
	}
	if log != "stdout marker\nstderr marker\n" {
		t.Fatalf("command log=%q", log)
	}
	if !strings.Contains(terminalErr.String(), "unrelated provider hint") || terminalOut.String() != "stdout marker\n" {
		t.Fatal("terminal behavior changed")
	}
	if record.LeaseID != "lease-fixture" || record.CaptureScope != "workload" || record.RecordingState != "terminal" || record.Source != "local" {
		t.Fatalf("record=%+v", record)
	}
}

func TestRunObservationAdoptsExistingCaptureOmissions(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	id := "run_00000000000000000000000000000003"
	o, err := beginRunObservation(Config{RecordLocal: true, Provider: "fixture"}, id, []string{"true"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var log runLogBuffer
	io.WriteString(capturedRunLogWriter{buffer: &log}, "capture-only bytes")
	io.WriteString(&log, "retained stderr")
	o.adoptDirectLog(&log, true, false)
	o.Results(&TestResultSummary{Format: "junit", Tests: 2, Failures: 1}, "collected")
	o.finish(timingReport{Provider: "fixture", ExitCode: 23, RunStatus: RunStatusFailed, ErrorKind: RunErrorCommandExit}, true, ExitError{Code: 23, Message: "command failed"}, nil)
	record, text, results, err := readLocalHistory(id)
	if err != nil {
		t.Fatal(err)
	}
	if text != "retained stderr" || !record.LogTruncated || record.Stdout.Availability != "omitted" || record.LogFullSHA256 != log.Snapshot().FullSHA256 {
		t.Fatalf("capture metadata=%+v text=%q", record, text)
	}
	if results == nil || results.Tests != 2 || results.Failures != 1 || *record.ExitCode != 23 {
		t.Fatal("result/exit lost")
	}
}

func TestRunObservationFinalFailureOnlyWarns(t *testing.T) {
	for _, code := range []int{0, 23} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			var warnings bytes.Buffer
			id := "run_00000000000000000000000000000004"
			o, err := beginRunObservation(Config{RecordLocal: true, Provider: "fixture"}, id, []string{"true"}, &warnings)
			if err != nil {
				t.Fatal(err)
			}
			if err := o.writer.Close(); err != nil {
				t.Fatal(err)
			}
			report := timingReport{Provider: "fixture", ExitCode: code, RunStatus: RunStatusSucceeded}
			var primary error
			if code != 0 {
				report.RunStatus, report.ErrorKind = RunStatusFailed, RunErrorCommandExit
				primary = ExitError{Code: code, Message: "primary"}
			}
			o.finish(report, true, primary, nil)
			if report.ExitCode != code || !strings.Contains(warnings.String(), "may be incomplete") {
				t.Fatalf("report=%+v warning=%q", report, warnings.String())
			}
			record, _, _, err := readLocalHistory(id)
			if err != nil || record.RecordingState != "incomplete" {
				t.Fatalf("record=%+v err=%v", record, err)
			}
		})
	}
}

func TestRunObservationNeverEntersWireRequest(t *testing.T) {
	plain, err := json.Marshal(RunRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	observed, err := json.Marshal(RunRequest{Command: []string{"true"}, Observation: &RunObservation{warnings: io.Discard}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain, observed) {
		t.Fatalf("local observation entered request: %s", observed)
	}
}

func TestRunObservationCoordinatorReferenceExcludesCredentials(t *testing.T) {
	base := &url.URL{Scheme: "https", Host: "EXAMPLE.invalid:443", Path: "/coordinator/", User: url.UserPassword("fixture", "fixture"), RawQuery: "sample=ignored", Fragment: "ignored"}
	got := coordinatorHistoryReference(base.String())
	if got == "" || got != coordinatorHistoryReference("https://example.invalid/coordinator") {
		t.Fatal("noncredential endpoint identity differs")
	}
	if got == coordinatorHistoryReference("https://example.invalid/other") {
		t.Fatal("basepath identity lost")
	}
}

func TestRunObservationCoordinatorTerminalAcknowledgment(t *testing.T) {
	for _, mode := range []string{"failed-event", "verified-receipt", "missing-receipt"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			receipt := runRecorderTestReceipt(t)
			client := &CoordinatorClient{BaseURL: "https://example.invalid", Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				code, body := http.StatusOK, `{"run":{"id":"run_123"}}`
				switch {
				case strings.HasSuffix(req.URL.Path, "/events") && mode == "failed-event":
					code, body = http.StatusBadRequest, `{"error":"ordinary fixture error"}`
				case strings.HasSuffix(req.URL.Path, "/finish"):
				case strings.HasSuffix(req.URL.Path, "/receipt"):
					if mode == "verified-receipt" {
						data, err := json.Marshal(map[string]any{"receipt": receipt})
						if err != nil {
							t.Fatal(err)
						}
						body = string(data)
					} else {
						code, body = http.StatusNotFound, `{"error":"not recorded"}`
					}
				default:
					t.Fatalf("unexpected request %s", req.URL.Path)
				}
				return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}}
			recorder := &runRecorder{coord: client, runID: "run_123", stderr: io.Discard}
			if mode == "failed-event" {
				recorder.Failed(fmt.Errorf("ordinary command failure"))
				if !recorder.finished {
					t.Fatal("existing failure lifecycle changed")
				}
			} else {
				err := recorder.Finish(t.Context(), SSHTarget{}, 1, 0, 0, "", false, nil, FailureClassification{}, &receipt)
				if (err == nil) != (mode == "verified-receipt") {
					t.Fatalf("Finish=%v", err)
				}
			}
			id := "run_00000000000000000000000000000005"
			o, err := beginRunObservation(Config{RecordLocal: true}, id, []string{"true"}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			o.finish(timingReport{ExitCode: 1}, true, nil, recorder)
			record, _, _, err := readLocalHistory(id)
			if err != nil {
				t.Fatal(err)
			}
			wantSource, wantState := "local", "unknown"
			if mode == "verified-receipt" {
				wantSource, wantState = "both", "recorded"
			}
			if record.Source != wantSource || record.CoordinatorState != wantState {
				t.Fatalf("source=%s coordinator=%s", record.Source, record.CoordinatorState)
			}
		})
	}
}

func TestRunObservationPolicyIgnoresRepositoryFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("history:\n  local:\n    enabled: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := readFileConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	if cfg.RecordLocal {
		t.Fatal("repository enabled history")
	}
	if err := applyFileConfigWithTrust(&cfg, file, true); err != nil {
		t.Fatal(err)
	}
	if !cfg.RecordLocal {
		t.Fatal("trusted user policy not applied")
	}
}
