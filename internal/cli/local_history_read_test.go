package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func readerLocalRecord(t *testing.T, modify ...func(*localHistoryRecord)) localHistoryRecord {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now().UTC().Format(time.RFC3339Nano)
	r := localHistoryRecord{Version: 1, ID: "run_0123456789abcdef0123456789abcdef", RecordingState: "active", Source: "local", CoordinatorState: "none", Provider: "local-container", CommandDisplay: "test", Phase: "admitted", StartedAt: now, UpdatedAt: now, ResultsState: "collected", CaptureScope: "workload", Stdout: localHistoryStream{Availability: "retained"}, Stderr: localHistoryStream{Availability: "omitted", Reason: "capture-only"}}
	for _, f := range modify {
		f(&r)
	}
	writer, err := beginLocalHistory(r)
	if err != nil {
		t.Fatal(err)
	}
	r.RecordingState = "terminal"
	r.Phase = "finalizing"
	r.EndedAt = now
	code := 0
	r.ExitCode = &code
	err = writer.Commit(r, runLogSnapshot{Log: "first\nlast\n", Truncated: true}, &TestResultSummary{Format: "junit", Tests: 3, Failures: 1})
	closeErr := writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return r
}

func TestLocalHistoryReadersOffline(t *testing.T) {
	r := readerLocalRecord(t)
	// Explicit local must not even parse an invalid coordinator configuration.
	t.Setenv("CRABBOX_COORDINATOR_TOKEN_COMMAND", "not-json")
	var out, diag bytes.Buffer
	a := App{Stdout: &out, Stderr: &diag}
	if err := a.logs(context.Background(), []string{r.ID, "--source", "local", "--tail", "1"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "last\n" {
		t.Fatalf("log=%q", out.String())
	}
	if !strings.Contains(diag.String(), "stderr=omitted") || !strings.Contains(diag.String(), "capture-only") || !strings.Contains(diag.String(), "source=local") {
		t.Fatalf("diagnostics=%q", diag.String())
	}
	out.Reset()
	diag.Reset()
	if err := a.results(context.Background(), []string{r.ID, "--source", "local"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "tests=3 failures=1") {
		t.Fatalf("results=%q", out.String())
	}
	out.Reset()
	if err := a.history(context.Background(), []string{"--source", "local", "--json"}); err != nil {
		t.Fatal(err)
	}
	var e historyReadEnvelope
	if err := json.Unmarshal(out.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if len(e.Records) != 1 || e.Records[0].Local.ID != r.ID || e.Records[0].Coordinator != nil {
		t.Fatalf("envelope=%+v", e)
	}
}

func TestLocalHistoryAllPreservesUnacknowledgedCollision(t *testing.T) {
	r := readerLocalRecord(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"runs": []CoordinatorRun{{ID: r.ID, StartedAt: r.StartedAt}}})
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	var out, diag bytes.Buffer
	a := App{Stdout: &out, Stderr: &diag}
	if err := a.history(context.Background(), []string{"--source", "all", "--json"}); err != nil {
		t.Fatal(err)
	}
	var e historyReadEnvelope
	if err := json.Unmarshal(out.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if len(e.Records) != 2 || e.Records[0].Source == "both" || e.Records[1].Source == "both" {
		t.Fatalf("collision=%+v", e)
	}
}

func TestLocalHistoryAllReportsSourceFailure(t *testing.T) {
	readerLocalRecord(t)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN_COMMAND", "not-json")
	var out, diag bytes.Buffer
	a := App{Stdout: &out, Stderr: &diag}
	if err := a.history(context.Background(), []string{"--source", "all", "--json"}); err == nil {
		t.Fatal("expected source failure")
	}
	var e historyReadEnvelope
	if err := json.Unmarshal(out.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if len(e.Records) != 1 || e.Errors["coordinator"] == "" {
		t.Fatalf("partial=%+v", e)
	}
}

func TestLocalHistoryReaderMaintenance(t *testing.T) {
	r := readerLocalRecord(t)
	var out, diag bytes.Buffer
	a := App{Stdout: &out, Stderr: &diag}
	if err := a.history(context.Background(), []string{"delete", r.ID, "--source", "coordinator"}); err == nil {
		t.Fatal("remote delete accepted")
	}
	if _, _, _, err := readLocalHistory(r.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.history(context.Background(), []string{"delete", r.ID, "--json"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readLocalHistory(r.ID); err == nil {
		t.Fatal("record survived delete")
	}
}

func TestLocalHistoryReaderDefaultWithoutCoordinator(t *testing.T) {
	r := readerLocalRecord(t)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN_COMMAND", "")
	var out, diag bytes.Buffer
	a := App{Stdout: &out, Stderr: &diag}
	if err := a.logs(context.Background(), []string{r.ID}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "first\nlast\n" {
		t.Fatalf("log=%q", out.String())
	}
}

func TestLocalHistoryReaderEmptyDoesNotCreateStore(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	var out, diag bytes.Buffer
	a := App{Stdout: &out, Stderr: &diag}
	if err := a.history(context.Background(), []string{"--source", "local", "--json"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("read created entries: %v", entries)
	}
}

func TestLocalHistoryAllCoalescesOnlyAcknowledgedReference(t *testing.T) {
	for _, binding := range []string{"matching", "different", "missing"} {
		t.Run(binding, func(t *testing.T) {
			var r localHistoryRecord
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"runs": []CoordinatorRun{{ID: r.ID, StartedAt: r.StartedAt}}})
			}))
			defer server.Close()
			r = readerLocalRecord(t, func(r *localHistoryRecord) {
				r.CoordinatorState = "recorded"
				r.CoordinatorRunID = r.ID
				r.Source = "both"
				switch binding {
				case "matching":
					r.CoordinatorReference = coordinatorHistoryReference(server.URL)
				case "different":
					r.CoordinatorReference = coordinatorHistoryReference("https://other.example.invalid/broker")
				}
			})
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			var out, diag bytes.Buffer
			a := App{Stdout: &out, Stderr: &diag}
			if err := a.history(context.Background(), []string{"--source", "all", "--json"}); err != nil {
				t.Fatal(err)
			}
			var e historyReadEnvelope
			if err := json.Unmarshal(out.Bytes(), &e); err != nil {
				t.Fatal(err)
			}
			if binding == "matching" {
				if len(e.Records) != 1 || e.Records[0].Source != "both" || e.Records[0].Local == nil || e.Records[0].Coordinator == nil {
					t.Fatalf("acknowledged=%+v", e)
				}
			} else if len(e.Records) != 2 {
				t.Fatalf("unbound records coalesced: %+v", e)
			}
			out.Reset()
			if err := a.history(context.Background(), []string{"--source", "local"}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "source=local provenance=both coordinator=recorded") {
				t.Fatalf("provenance missing: %q", out.String())
			}
		})
	}
}

func TestLocalHistoryReaderDeleteRejectsExtraIDs(t *testing.T) {
	r := readerLocalRecord(t)
	var out, diag bytes.Buffer
	a := App{Stdout: &out, Stderr: &diag}
	for _, args := range [][]string{
		{"delete", r.ID, "extra"},
		{"delete", r.ID, "--source", "local", "extra"},
		{"delete", "--source", "local", r.ID, "extra"},
	} {
		if err := a.history(context.Background(), args); err == nil {
			t.Fatalf("accepted extra ID: %v", args)
		}
		if _, _, _, err := readLocalHistory(r.ID); err != nil {
			t.Fatalf("invalid command deleted record: %v", err)
		}
	}
}

func TestLocalHistoryReaderSortsInstantsAndTies(t *testing.T) {
	rows := []historyReadRow{
		{Source: "local", Local: &localHistoryRecord{ID: "b", StartedAt: "2026-09-12T10:00:00Z"}},
		{Source: "coordinator", Coordinator: &CoordinatorRun{ID: "fraction", StartedAt: "2026-09-12T10:00:00.1Z"}},
		{Source: "coordinator", Coordinator: &CoordinatorRun{ID: "offset", StartedAt: "2026-09-12T09:00:00-02:00"}},
		{Source: "local", Local: &localHistoryRecord{ID: "a", StartedAt: "2026-09-12T12:00:00+02:00"}},
		{Source: "coordinator", Coordinator: &CoordinatorRun{ID: "a", StartedAt: "2026-09-12T10:00:00.000Z"}},
		{Source: "coordinator", Coordinator: &CoordinatorRun{ID: "invalid", StartedAt: "unknown"}},
	}
	sort.SliceStable(rows, func(i, j int) bool { return historyRowBefore(rows[i], rows[j]) })
	want := []string{"offset/coordinator", "fraction/coordinator", "a/coordinator", "a/local", "b/local", "invalid/coordinator"}
	for i, row := range rows {
		if got := historyRowID(row) + "/" + row.Source; got != want[i] {
			t.Fatalf("row %d=%s want %s", i, got, want[i])
		}
	}
}
