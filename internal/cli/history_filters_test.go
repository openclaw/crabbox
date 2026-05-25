package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogsTailFlag(t *testing.T) {
	clearConfigEnv(t)
	setTestHome(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs/run_123/logs" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte("one\ntwo\nthree\n"))
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).logs(context.Background(), []string{"run_123", "--tail", "2"})
	if err != nil {
		t.Fatalf("logs error=%v stderr=%q", err, stderr.String())
	}
	if got := stdout.String(); got != "two\nthree\n" {
		t.Fatalf("tail output=%q", got)
	}
}

func TestEventsTypeAndPhaseFlags(t *testing.T) {
	clearConfigEnv(t)
	setTestHome(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs/run_123/events" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"events": []CoordinatorRunEvent{
			{RunID: "run_123", Seq: 1, Type: "stdout", Phase: "command", Data: "ok\n", CreatedAt: "2026-05-01T00:00:00Z"},
			{RunID: "run_123", Seq: 2, Type: "stderr", Phase: "command", Data: "fail\n", CreatedAt: "2026-05-01T00:00:01Z"},
			{RunID: "run_123", Seq: 3, Type: "lease.released", Phase: "released", CreatedAt: "2026-05-01T00:00:02Z"},
		}})
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).events(context.Background(), []string{"run_123", "--type", "stderr", "--phase", "command"})
	if err != nil {
		t.Fatalf("events error=%v stderr=%q", err, stderr.String())
	}
	text := stdout.String()
	if !strings.Contains(text, "stderr") || strings.Contains(text, "stdout") || strings.Contains(text, "lease.released") {
		t.Fatalf("filtered events output=%q", text)
	}
}

func TestEventsTypeFilterPaginatesUntilMatch(t *testing.T) {
	clearConfigEnv(t)
	setTestHome(t)
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs/run_123/events" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		requests++
		after := r.URL.Query().Get("after")
		events := make([]CoordinatorRunEvent, 0, 500)
		if after == "" {
			for seq := 1; seq <= 500; seq++ {
				events = append(events, CoordinatorRunEvent{RunID: "run_123", Seq: seq, Type: "stdout", Phase: "command", Data: "ok\n", CreatedAt: "2026-05-01T00:00:00Z"})
			}
		} else if after == "500" {
			events = append(events, CoordinatorRunEvent{RunID: "run_123", Seq: 501, Type: "stderr", Phase: "command", Data: "fail\n", CreatedAt: "2026-05-01T00:00:01Z"})
		} else {
			t.Fatalf("unexpected after=%q", after)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"events": events})
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).events(context.Background(), []string{"run_123", "--type", "stderr"})
	if err != nil {
		t.Fatalf("events error=%v stderr=%q", err, stderr.String())
	}
	if requests != 2 {
		t.Fatalf("requests=%d want 2", requests)
	}
	text := stdout.String()
	if !strings.Contains(text, "stderr") || strings.Contains(text, "stdout") {
		t.Fatalf("filtered events output=%q", text)
	}
}

func TestResultsFailedOnlyFlag(t *testing.T) {
	clearConfigEnv(t)
	setTestHome(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs/run_123" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"run": CoordinatorRun{
			ID:      "run_123",
			Command: []string{"go", "test"},
			Results: &TestResultSummary{
				Format:   "junit",
				Tests:    2,
				Failures: 1,
				Failed: []TestFailure{{
					Suite:   "pkg",
					Name:    "TestFails",
					Kind:    "failure",
					Message: "want ok\nstack",
				}},
			},
		}})
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).results(context.Background(), []string{"run_123", "--failed-only"})
	if err != nil {
		t.Fatalf("results error=%v stderr=%q", err, stderr.String())
	}
	text := stdout.String()
	if strings.Contains(text, "results format=") || !strings.Contains(text, "pkg failure") || !strings.Contains(text, "want ok") {
		t.Fatalf("failed-only output=%q", text)
	}
}

func TestResultsFailedOnlyJSONUsesEmptyArray(t *testing.T) {
	clearConfigEnv(t)
	setTestHome(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs/run_123" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"run": CoordinatorRun{
			ID:      "run_123",
			Command: []string{"go", "test"},
			Results: &TestResultSummary{
				Format: "junit",
				Tests:  1,
			},
		}})
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).results(context.Background(), []string{"run_123", "--failed-only", "--json"})
	if err != nil {
		t.Fatalf("results error=%v stderr=%q", err, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "[]" {
		t.Fatalf("failed-only json=%q", got)
	}
}

func setTestHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(home, "missing.yaml"))
}
