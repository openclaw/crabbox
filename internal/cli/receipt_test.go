package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReceiptCommandRecoversCommittedReceiptAfterLostFinishResponse(t *testing.T) {
	setAttestTestHome(t)
	startedAt := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	endedAt := startedAt.Add(2 * time.Second)
	logText := "failed\n"
	receipt, err := buildTerminalRunReceipt("", terminalRunReceiptInput{
		Provider:          "aws",
		LeaseID:           "cbx_abc123",
		Slug:              "blue-lobster",
		RunID:             "run_123",
		Command:           []string{"go", "test", "./..."},
		CommandDisplay:    "go test ./...",
		ExitCode:          1,
		SyncMs:            100,
		CommandMs:         1900,
		StartedAt:         startedAt,
		EndedAt:           endedAt,
		LogSHA256:         sha256Digest([]byte(logText)),
		RetainedLogSHA256: sha256Digest([]byte(logText)),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/runs/run_123":
			_ = json.NewEncoder(w).Encode(map[string]any{"run": CoordinatorRun{
				ID:           "run_123",
				LeaseID:      "cbx_abc123",
				Slug:         "blue-lobster",
				Provider:     "aws",
				Command:      []string{"go", "test", "./..."},
				State:        "failed",
				ExitCode:     intPointer(1),
				SyncMs:       100,
				CommandMs:    1900,
				DurationMs:   2000,
				LogBytes:     int64(len(logText)),
				LogTruncated: false,
				StartedAt:    startedAt.Format(time.RFC3339Nano),
				EndedAt:      endedAt.Format(time.RFC3339Nano),
			}})
		case "/v1/runs/run_123/receipt":
			_ = json.NewEncoder(w).Encode(map[string]any{"receipt": receipt})
		case "/v1/runs/run_123/logs":
			_, _ = w.Write([]byte(logText))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.receipt(context.Background(), []string{"run_123", "--expected-signer", receipt.Signer}); err != nil {
		t.Fatalf("receipt: %v stderr=%s", err, stderr.String())
	}
	var recovered terminalRunReceipt
	if err := json.Unmarshal(stdout.Bytes(), &recovered); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if recovered.Signature != receipt.Signature || recovered.ExitCode != 1 {
		t.Fatalf("recovered receipt=%#v", recovered)
	}

	stdout.Reset()
	err = app.receipt(context.Background(), []string{"run_123", "--expected-signer", "sha256:" + strings.Repeat("0", 64)})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 1 || !strings.Contains(exitErr.Message, "signer mismatch") {
		t.Fatalf("expected signer mismatch, got %v", err)
	}
}

func TestReceiptCommandLeavesMissingReceiptAmbiguous(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/runs/run_123":
			_, _ = w.Write([]byte(`{"run":{"id":"run_123","leaseID":"cbx_abc123","owner":"alice@example.com","org":"example-org","provider":"aws","class":"standard","serverType":"t3.small","command":["false"],"state":"failed","exitCode":1,"logBytes":0,"logTruncated":false,"startedAt":"2026-08-23T10:00:00Z","endedAt":"2026-08-23T10:00:01Z"}}`))
		case "/v1/runs/run_123/receipt":
			http.NotFound(w, r)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.receipt(context.Background(), []string{"run_123"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 4 || !strings.Contains(exitErr.Message, "execution remains ambiguous") {
		t.Fatalf("error=%v", err)
	}
}

func intPointer(value int) *int {
	return &value
}
