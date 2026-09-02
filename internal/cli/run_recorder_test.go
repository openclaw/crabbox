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
	"testing"
	"time"
)

func TestRunRecorderCapturesTelemetryOnlyWithRunHandle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell ssh fixture")
	}
	for _, test := range []struct {
		name        string
		coordinator bool
		runID       string
	}{
		{name: "direct"},
		{name: "awaiting run handle", coordinator: true},
		{name: "recorded", coordinator: true, runID: "run_123"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			callsPath := filepath.Join(dir, "calls")
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nprintf 'telemetry\\n' >> \"$CRABBOX_FAKE_TELEMETRY_CALLS\"\nprintf 'cpuCount=2\\n'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_FAKE_TELEMETRY_CALLS", callsPath)
			posts := 0
			var client *CoordinatorClient
			if test.coordinator {
				client = &CoordinatorClient{BaseURL: "https://example.test", Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.Method != http.MethodPost || req.URL.Path != "/v1/runs/run_123/telemetry" {
						t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
					}
					var body struct {
						Telemetry LeaseTelemetry `json:"telemetry"`
					}
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.Telemetry.CPUCount == nil || *body.Telemetry.CPUCount != 2 {
						t.Fatalf("telemetry=%+v error=%v", body.Telemetry, err)
					}
					posts++
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"run":{"id":"run_123"}}`)), Header: make(http.Header)}, nil
				})}}
			}
			rec := newRunRecorder(t.Context(), client, Config{}, []string{"true"}, "", io.Discard, true)
			rec.runID = test.runID
			target := SSHTarget{User: "runner", Host: "example.test", Port: "22", FallbackPorts: []string{}}
			rec.CaptureTelemetryStart(t.Context(), target)
			rec.CaptureTelemetryStart(t.Context(), target)
			calls, err := os.ReadFile(callsPath)
			if test.runID == "" {
				if !os.IsNotExist(err) || posts != 0 || rec.telemetryStart != nil || len(rec.telemetrySnapshot()) != 0 {
					t.Fatalf("unrecorded run collected telemetry: SSH=%q posts=%d error=%v", calls, posts, err)
				}
			} else if err != nil || string(calls) != "telemetry\n" || posts != 1 || len(rec.telemetrySnapshot()) != 1 {
				t.Fatalf("recorded run needs one start sample: SSH=%q posts=%d error=%v", calls, posts, err)
			}
		})
	}
}

func TestRunRecorderRedactsCoordinatorDiagnosticEvents(t *testing.T) {
	const (
		configuredSecret = "configured-provider-fixture-value"
		runtimeSecret    = "runtime-provider-fixture-value"
	)
	t.Setenv("AWS_SESSION_TOKEN", runtimeSecret)

	message := strings.Join([]string{
		"provider request failed region=eu",
		"configured=" + configuredSecret,
		"runtime=" + runtimeSecret,
		"Authorization: Bearer minted-provider-fixture-value",
		strings.Join([]string{
			"https://fixture-user",
			"fixture-password@example.test/path?token=query-fixture-value&region=eu",
		}, ":"),
		`{"clientSecret":"json-fixture-value","message":"quota exceeded"}`,
	}, "\n")

	for _, test := range []struct {
		name   string
		kind   string
		phase  string
		record func(*runRecorder)
	}{
		{
			name:  "hydration failure",
			kind:  "actions.hydrate.failed",
			phase: "hydrate",
			record: func(rec *runRecorder) {
				rec.Event("actions.hydrate.failed", "hydrate", message)
			},
		},
		{
			name:  "lease replacement failure",
			kind:  "lease.replace.failed",
			phase: "leasing",
			record: func(rec *runRecorder) {
				rec.Event("lease.replace.failed", "leasing", message)
			},
		},
		{
			name:  "terminal run failure",
			kind:  "run.failed",
			phase: "failed",
			record: func(rec *runRecorder) {
				rec.Failed(errors.New(message))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []CoordinatorRunEventInput
			client := &CoordinatorClient{
				BaseURL: "https://example.test",
				Client:  &http.Client{Transport: runEventRecordingRoundTripper{events: &events}},
			}
			rec := newRunRecorder(context.Background(), client, Config{
				Morph: MorphConfig{APIKey: configuredSecret},
			}, []string{"go", "test"}, "", io.Discard, true)
			rec.runID = "run_123"

			test.record(rec)

			if len(events) != 1 {
				t.Fatalf("events=%v, want one posted diagnostic", events)
			}
			event := events[0]
			if event.Type != test.kind || event.Phase != test.phase {
				t.Fatalf("event=%#v, want type=%q phase=%q", event, test.kind, test.phase)
			}
			for _, leaked := range []string{
				configuredSecret,
				runtimeSecret,
				"minted-provider-fixture-value",
				"fixture-user",
				"fixture-password",
				"query-fixture-value",
				"json-fixture-value",
			} {
				if strings.Contains(event.Message, leaked) {
					t.Fatalf("coordinator diagnostic leaked %q: %s", leaked, event.Message)
				}
			}
			for _, preserved := range []string{"provider request failed", "region=eu", "quota exceeded", diagnosticRedaction} {
				if !strings.Contains(event.Message, preserved) {
					t.Fatalf("coordinator diagnostic lost %q: %s", preserved, event.Message)
				}
			}
		})
	}
}

func TestRunRecorderPreservesRawStreamEventData(t *testing.T) {
	const configuredSecret = "configured-output-fixture-value"
	var events []CoordinatorRunEventInput
	client := &CoordinatorClient{
		BaseURL: "https://example.test",
		Client:  &http.Client{Transport: runEventRecordingRoundTripper{events: &events}},
	}
	rec := newRunRecorder(context.Background(), client, Config{
		Morph: MorphConfig{APIKey: configuredSecret},
	}, []string{"go", "test"}, "", io.Discard, true)
	rec.runID = "run_123"

	stdout := rec.StreamWriter("stdout")
	if _, err := stdout.Write([]byte("caller-owned output " + configuredSecret)); err != nil {
		t.Fatal(err)
	}
	stdout.Flush()
	rec.waitForOutputEvents(time.Second)

	if len(events) != 1 || events[0].Type != "stdout" {
		t.Fatalf("events=%#v, want one stdout event", events)
	}
	if !strings.Contains(events[0].Data, configuredSecret) {
		t.Fatalf("caller-owned stdout was unexpectedly rewritten: %#v", events[0])
	}
}

func TestRunRecorderRedactsRefreshedRuntimeDiagnosticSecrets(t *testing.T) {
	const (
		originalSecret  = "original-runtime-fixture-value"
		refreshedSecret = "refreshed-runtime-fixture-value"
	)
	t.Setenv("AWS_SESSION_TOKEN", originalSecret)

	var events []CoordinatorRunEventInput
	client := &CoordinatorClient{
		BaseURL: "https://example.test",
		Client:  &http.Client{Transport: runEventRecordingRoundTripper{events: &events}},
	}
	rec := newRunRecorder(context.Background(), client, Config{}, []string{"go", "test"}, "", io.Discard, true)
	rec.runID = "run_123"
	t.Setenv("AWS_SESSION_TOKEN", refreshedSecret)

	rec.Event("actions.hydrate.failed", "hydrate", "original="+originalSecret+" refreshed="+refreshedSecret+" region=eu")

	if len(events) != 1 {
		t.Fatalf("events=%#v, want one posted diagnostic", events)
	}
	for _, leaked := range []string{originalSecret, refreshedSecret} {
		if strings.Contains(events[0].Message, leaked) {
			t.Fatalf("runtime diagnostic leaked %q after refresh: %s", leaked, events[0].Message)
		}
	}
	if !strings.Contains(events[0].Message, "region=eu") {
		t.Fatalf("runtime diagnostic lost routing context: %s", events[0].Message)
	}
}

func TestRunRecorderRedactsDiagnosticSecretsAfterLateCoordinatorAttachment(t *testing.T) {
	const (
		configuredSecret = "late-configured-provider-fixture-value"
		originalSecret   = "late-original-runtime-fixture-value"
		refreshedSecret  = "late-refreshed-runtime-fixture-value"
	)
	t.Setenv("AWS_SESSION_TOKEN", originalSecret)

	rec := newRunRecorder(context.Background(), nil, Config{
		Morph: MorphConfig{APIKey: configuredSecret},
	}, []string{"go", "test"}, "", io.Discard, true)
	t.Setenv("AWS_SESSION_TOKEN", refreshedSecret)

	var events []CoordinatorRunEventInput
	rec.UseCoordinator(&CoordinatorClient{
		BaseURL: "https://example.test",
		Client:  &http.Client{Transport: runEventRecordingRoundTripper{events: &events}},
	})
	rec.runID = "run_123"
	rec.Event("actions.hydrate.failed", "hydrate", strings.Join([]string{
		"configured=" + configuredSecret,
		"original=" + originalSecret,
		"refreshed=" + refreshedSecret,
		"region=eu",
	}, " "))

	if len(events) != 1 {
		t.Fatalf("events=%#v, want one posted diagnostic", events)
	}
	for _, leaked := range []string{configuredSecret, originalSecret, refreshedSecret} {
		if strings.Contains(events[0].Message, leaked) {
			t.Fatalf("late-attached coordinator diagnostic leaked %q: %s", leaked, events[0].Message)
		}
	}
	if !strings.Contains(events[0].Message, "region=eu") {
		t.Fatalf("late-attached coordinator diagnostic lost routing context: %s", events[0].Message)
	}
}

func TestRunRecorderRedactsPersistedCoordinatorDiagnosticEvents(t *testing.T) {
	coordinatorURL := strings.TrimSpace(os.Getenv("CRABBOX_RUN_RECORDER_PROOF_URL"))
	if coordinatorURL == "" {
		t.Skip("set CRABBOX_RUN_RECORDER_PROOF_URL to verify a running coordinator")
	}

	const (
		configuredSecret = "persisted-configured-provider-fixture-value"
		originalSecret   = "persisted-original-runtime-fixture-value"
		refreshedSecret  = "persisted-refreshed-runtime-fixture-value"
	)
	t.Setenv("AWS_SESSION_TOKEN", originalSecret)
	cfg := Config{
		Provider:   "aws",
		Class:      "standard",
		ServerType: "t3.small",
		Morph:      MorphConfig{APIKey: configuredSecret},
	}
	client := &CoordinatorClient{
		BaseURL: coordinatorURL,
		Token:   os.Getenv("CRABBOX_RUN_RECORDER_PROOF_TOKEN"),
		Client:  &http.Client{Timeout: 10 * time.Second},
	}
	rec := newRunRecorder(context.Background(), nil, cfg, []string{"go", "test"}, "security-redaction-proof", io.Discard, true)
	t.Setenv("AWS_SESSION_TOKEN", refreshedSecret)
	rec.UseCoordinator(client)
	run, err := client.CreateRun(context.Background(), "", cfg, rec.command, rec.label)
	if err != nil {
		t.Fatalf("create coordinator run: %v", err)
	}
	rec.attachRun(run)
	rec.Event("actions.hydrate.failed", "hydrate", strings.Join([]string{
		"configured=" + configuredSecret,
		"original=" + originalSecret,
		"refreshed=" + refreshedSecret,
		"region=eu",
	}, " "))

	events, err := client.RunEvents(context.Background(), run.ID, 0, 20)
	if err != nil {
		t.Fatalf("read persisted coordinator events: %v", err)
	}
	for _, event := range events {
		if event.Type != "actions.hydrate.failed" {
			continue
		}
		for _, leaked := range []string{configuredSecret, originalSecret, refreshedSecret} {
			if strings.Contains(event.Message, leaked) {
				t.Fatalf("persisted coordinator event leaked %q: %s", leaked, event.Message)
			}
		}
		if !strings.Contains(event.Message, "region=eu") || !strings.Contains(event.Message, diagnosticRedaction) {
			t.Fatalf("persisted coordinator event lost diagnostic context: %s", event.Message)
		}
		t.Logf("persisted coordinator run=%s event=%s message=%q", run.ID, event.Type, event.Message)
		return
	}
	t.Fatalf("persisted coordinator events missing diagnostic: %#v", events)
}

func TestRunEventStreamWriterCapsOutputEvents(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "test@example.com")
	var events []CoordinatorRunEventInput
	client := &CoordinatorClient{
		BaseURL: "https://example.test",
		Client:  &http.Client{Transport: runEventRecordingRoundTripper{events: &events}},
	}
	rec := &runRecorder{coord: client, runID: "run_123", stderr: io.Discard}
	stdout := rec.StreamWriter("stdout")
	chunk := bytes.Repeat([]byte("x"), runEventOutputChunkBytes)
	for i := 0; i < runEventOutputMaxBytes/runEventOutputChunkBytes+10; i++ {
		n, err := stdout.Write(chunk)
		if err != nil {
			t.Fatal(err)
		}
		if n != len(chunk) {
			t.Fatalf("Write returned %d, want %d", n, len(chunk))
		}
	}
	stdout.Flush()
	rec.waitForOutputEvents(time.Second)

	var outputBytes, outputEvents, truncatedEvents int
	for _, event := range events {
		switch event.Type {
		case "stdout":
			outputEvents++
			outputBytes += len(event.Data)
			if len(event.Data) > runEventOutputChunkBytes {
				t.Fatalf("stdout event data length=%d, want <=%d", len(event.Data), runEventOutputChunkBytes)
			}
		case "output.truncated":
			truncatedEvents++
		default:
			t.Fatalf("unexpected event type %q", event.Type)
		}
	}
	if outputBytes != runEventOutputMaxBytes {
		t.Fatalf("outputBytes=%d, want %d", outputBytes, runEventOutputMaxBytes)
	}
	if outputEvents != runEventOutputMaxBytes/runEventOutputChunkBytes {
		t.Fatalf("outputEvents=%d, want %d", outputEvents, runEventOutputMaxBytes/runEventOutputChunkBytes)
	}
	if truncatedEvents != 1 {
		t.Fatalf("truncatedEvents=%d, want 1", truncatedEvents)
	}

	before := len(events)
	if _, err := stdout.Write(chunk); err != nil {
		t.Fatal(err)
	}
	stdout.Flush()
	if len(events) != before {
		t.Fatalf("events after cap=%d, want %d", len(events), before)
	}
}

func TestRunEventStreamWriterDoesNotBlockOnCoordinatorPost(t *testing.T) {
	started := make(chan struct{})
	client := &CoordinatorClient{
		BaseURL: "https://example.test",
		Client:  &http.Client{Transport: blockingRoundTripper{started: started}},
	}
	rec := &runRecorder{coord: client, runID: "run_123", stderr: io.Discard}
	stdout := rec.StreamWriter("stdout")
	chunk := bytes.Repeat([]byte("x"), runEventOutputChunkBytes)

	start := time.Now()
	n, err := stdout.Write(chunk)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(chunk) {
		t.Fatalf("Write returned %d, want %d", n, len(chunk))
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("Write blocked for %s", elapsed)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("output event post did not start")
	}
}

func TestRunRecorderDefersCreateWhenCoordinatorRequiresLeaseID(t *testing.T) {
	var stderr bytes.Buffer
	var createBodies []map[string]any
	var eventBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			createBodies = append(createBodies, body)
			if body["leaseID"] == "" {
				http.Error(w, `{"error":"invalid_lease_id"}`, http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"run":{"id":"run_123","leaseID":"cbx_abcdef123456","owner":"peter@example.com","org":"openclaw","provider":"aws","class":"standard","serverType":"t3.small","command":["pnpm","test"],"state":"running","phase":"starting","logBytes":0,"logTruncated":false,"startedAt":"2026-05-02T00:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/run_123/events":
			if err := json.NewDecoder(r.Body).Decode(&eventBody); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"event":{"runID":"run_123","seq":1,"type":"lease.created","createdAt":"2026-05-02T00:00:01Z"}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	rec := newRunRecorder(context.Background(), client, Config{
		Provider:   "aws",
		Class:      "standard",
		ServerType: "t3.small",
	}, []string{"pnpm", "test"}, "", &stderr, false)
	rec.Event("leasing.started", "leasing", "")
	rec.AttachLease("cbx_abcdef123456", "blue-lobster", Config{
		Provider:   "aws",
		Class:      "standard",
		ServerType: "t3.small",
	})

	if len(createBodies) != 2 {
		t.Fatalf("create requests=%d want 2", len(createBodies))
	}
	if got := createBodies[0]["leaseID"]; got != "" {
		t.Fatalf("first create leaseID=%#v want empty", got)
	}
	if got := createBodies[1]["leaseID"]; got != "cbx_abcdef123456" {
		t.Fatalf("second create leaseID=%#v", got)
	}
	if got := eventBody["type"]; got != "lease.created" {
		t.Fatalf("event body=%#v", eventBody)
	}
	if text := stderr.String(); strings.Contains(text, "warning:") || !strings.Contains(text, "recording run run_123") {
		t.Fatalf("stderr=%q", text)
	}
	selection := runEnvSelection{Inline: map[string]string{}, Effective: map[string]string{}}
	applyRunExecutionMetadata(&selection, "cbx_abcdef123456", rec.runID, "blue-lobster")
	if selection.Effective[runEnvRunID] != "run_123" {
		t.Fatalf("execution metadata run ID=%q, want coordinator-issued run_123", selection.Effective[runEnvRunID])
	}
}

func TestRunRecorderHistoryAvailabilityRequiresRecordedRunID(t *testing.T) {
	if !((*runRecorder)(nil)).historyIsUnavailable() {
		t.Fatal("nil recorder reported history available")
	}
	if !(&runRecorder{}).historyIsUnavailable() {
		t.Fatal("local-only recorder reported history available")
	}
	if (&runRecorder{runID: "run_123"}).historyIsUnavailable() {
		t.Fatal("recorded coordinator run reported history unavailable")
	}
	if !(&runRecorder{runID: "run_123", historyUnavailable: true}).historyIsUnavailable() {
		t.Fatal("failed coordinator history reported available")
	}
}

func TestRunRecorderDefersCreateForExplicitLeaseRuns(t *testing.T) {
	var stderr bytes.Buffer
	var createBodies []map[string]any
	var eventBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			createBodies = append(createBodies, body)
			_, _ = w.Write([]byte(`{"run":{"id":"run_123","leaseID":"cbx_abcdef123456","owner":"bob@example.com","org":"elsewhere","provider":"aws","class":"standard","serverType":"t3.small","command":["pnpm","test"],"state":"running","phase":"starting","logBytes":0,"logTruncated":false,"startedAt":"2026-05-02T00:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/run_123/events":
			if err := json.NewDecoder(r.Body).Decode(&eventBody); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"event":{"runID":"run_123","seq":1,"type":"lease.created","leaseID":"cbx_abcdef123456","createdAt":"2026-05-02T00:00:01Z"}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	rec := newRunRecorder(context.Background(), client, Config{
		Provider:   "aws",
		Class:      "standard",
		ServerType: "t3.small",
	}, []string{"pnpm", "test"}, "", &stderr, true)
	rec.Event("leasing.started", "leasing", "")
	if len(createBodies) != 0 {
		t.Fatalf("create requests before lease=%d want 0", len(createBodies))
	}

	rec.AttachLease("cbx_abcdef123456", "blue-lobster", Config{
		Provider:   "aws",
		Class:      "standard",
		ServerType: "t3.small",
	})

	if len(createBodies) != 1 {
		t.Fatalf("create requests=%d want 1", len(createBodies))
	}
	if got := createBodies[0]["leaseID"]; got != "cbx_abcdef123456" {
		t.Fatalf("create leaseID=%#v", got)
	}
	if got := eventBody["type"]; got != "lease.created" {
		t.Fatalf("event body=%#v", eventBody)
	}
	if text := stderr.String(); strings.Contains(text, "warning:") || !strings.Contains(text, "recording run run_123") {
		t.Fatalf("stderr=%q", text)
	}
}

func TestRunRecorderRetriesTransientCreateFailureAfterLease(t *testing.T) {
	var stderr bytes.Buffer
	var createBodies []map[string]any
	var eventBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			createBodies = append(createBodies, body)
			if len(createBodies) == 1 {
				http.Error(w, `{"error":"temporary_unavailable"}`, http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"run":{"id":"run_123","leaseID":"cbx_abcdef123456","owner":"alice@example.com","org":"example-org","provider":"aws","class":"standard","serverType":"t3.small","command":["go","test"],"state":"running","phase":"starting","logBytes":0,"logTruncated":false,"startedAt":"2026-05-02T00:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/run_123/events":
			if err := json.NewDecoder(r.Body).Decode(&eventBody); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"event":{"runID":"run_123","seq":1,"type":"lease.created","createdAt":"2026-05-02T00:00:01Z"}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	rec := newRunRecorder(context.Background(), client, Config{
		Provider:   "aws",
		Class:      "standard",
		ServerType: "t3.small",
	}, []string{"go", "test"}, "", &stderr, false)
	rec.Event("leasing.started", "leasing", "")
	rec.AttachLease("cbx_abcdef123456", "blue-lobster", Config{
		Provider:   "aws",
		Class:      "standard",
		ServerType: "t3.small",
	})

	if len(createBodies) != 2 {
		t.Fatalf("create requests=%d want 2", len(createBodies))
	}
	if got := createBodies[0]["leaseID"]; got != "" {
		t.Fatalf("first create leaseID=%#v want empty", got)
	}
	if got := createBodies[1]["leaseID"]; got != "cbx_abcdef123456" {
		t.Fatalf("second create leaseID=%#v", got)
	}
	if got := eventBody["type"]; got != "lease.created" {
		t.Fatalf("event body=%#v", eventBody)
	}
	text := stderr.String()
	for _, want := range []string{
		"warning: run history create failed before lease; will retry after lease is available:",
		"recording run run_123",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stderr missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "run history unavailable") || rec.historyUnavailable {
		t.Fatalf("history should have recovered, stderr=%q unavailable=%v", text, rec.historyUnavailable)
	}
}

func TestRunRecorderMarksHistoryUnavailableAfterPersistentCreateFailure(t *testing.T) {
	var stderr bytes.Buffer
	var createBodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runs" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		createBodies = append(createBodies, body)
		if len(createBodies) == 1 {
			http.Error(w, `{"error":"temporary_unavailable"}`, http.StatusInternalServerError)
			return
		}
		http.Error(w, `{"error":"still_unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	rec := newRunRecorder(context.Background(), client, Config{
		Provider:   "aws",
		Class:      "standard",
		ServerType: "t3.small",
	}, []string{"go", "test"}, "", &stderr, false)
	rec.AttachLease("cbx_abcdef123456", "blue-lobster", Config{
		Provider:   "aws",
		Class:      "standard",
		ServerType: "t3.small",
	})

	if len(createBodies) != 2 {
		t.Fatalf("create requests=%d want 2", len(createBodies))
	}
	if rec.runID != "" || !rec.historyUnavailable {
		t.Fatalf("recorder runID=%q historyUnavailable=%v", rec.runID, rec.historyUnavailable)
	}
	text := stderr.String()
	for _, want := range []string{
		"warning: run history create failed before lease; will retry after lease is available:",
		"warning: run history create failed after lease; run history unavailable, use lease-based recovery commands:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stderr missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "recording run") {
		t.Fatalf("stderr should not record a run:\n%s", text)
	}
}

func TestRunRecorderRetriesFailedLeaseCreateOnReplacementLease(t *testing.T) {
	var stderr bytes.Buffer
	var createBodies []map[string]any
	var eventBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			createBodies = append(createBodies, body)
			if len(createBodies) < 3 {
				http.Error(w, `{"error":"temporary_unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"run":{"id":"run_123","leaseID":"cbx_replacement123","owner":"alice@example.com","org":"example-org","provider":"aws","class":"standard","serverType":"t3.small","command":["go","test"],"state":"running","phase":"starting","logBytes":0,"logTruncated":false,"startedAt":"2026-05-02T00:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/run_123/events":
			if err := json.NewDecoder(r.Body).Decode(&eventBody); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"event":{"runID":"run_123","seq":1,"type":"lease.created","createdAt":"2026-05-02T00:00:01Z"}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	rec := newRunRecorder(context.Background(), client, Config{
		Provider:   "aws",
		Class:      "standard",
		ServerType: "t3.small",
	}, []string{"go", "test"}, "", &stderr, false)
	rec.AttachLease("cbx_initial123456", "blue-lobster", Config{
		Provider:   "aws",
		Class:      "standard",
		ServerType: "t3.small",
	})
	if rec.runID != "" || !rec.createPending || !rec.historyUnavailable {
		t.Fatalf("after failed attach runID=%q createPending=%v historyUnavailable=%v", rec.runID, rec.createPending, rec.historyUnavailable)
	}

	rec.AttachLease("cbx_replacement123", "green-lobster", Config{
		Provider:   "aws",
		Class:      "standard",
		ServerType: "t3.small",
	})

	if len(createBodies) != 3 {
		t.Fatalf("create requests=%d want 3", len(createBodies))
	}
	if got := createBodies[1]["leaseID"]; got != "cbx_initial123456" {
		t.Fatalf("first lease-time create leaseID=%#v", got)
	}
	if got := createBodies[2]["leaseID"]; got != "cbx_replacement123" {
		t.Fatalf("replacement create leaseID=%#v", got)
	}
	if got := eventBody["leaseID"]; got != "cbx_replacement123" {
		t.Fatalf("lease.created body=%#v", eventBody)
	}
	if rec.runID != "run_123" || rec.createPending || rec.historyUnavailable {
		t.Fatalf("recovered recorder runID=%q createPending=%v historyUnavailable=%v", rec.runID, rec.createPending, rec.historyUnavailable)
	}
	text := stderr.String()
	for _, want := range []string{
		"warning: run history create failed before lease; will retry after lease is available:",
		"warning: run history create failed after lease; run history unavailable, use lease-based recovery commands:",
		"recording run run_123",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stderr missing %q:\n%s", want, text)
		}
	}
}

func TestRunRecorderAttachLeaseUsesResolvedCoordinator(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runs/run_123/events" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"event":{"runID":"run_123","seq":1,"type":"lease.created","createdAt":"2026-05-02T00:00:01Z"}}`))
	}))
	defer server.Close()

	rec := &runRecorder{
		coord:  &CoordinatorClient{BaseURL: server.URL, Token: "user-token", Client: server.Client()},
		runID:  "run_123",
		stderr: io.Discard,
	}
	rec.UseCoordinator(&CoordinatorClient{
		BaseURL: server.URL,
		Token:   "admin-token",
		Client:  server.Client(),
	})
	rec.AttachLease("cbx_abcdef123456", "blue-lobster", Config{
		Provider:   "aws",
		Class:      "standard",
		ServerType: "t3.small",
	})

	if authorization != "Bearer admin-token" {
		t.Fatalf("authorization=%q, want resolved admin token", authorization)
	}
}

func TestRunRecorderSuppressesMissingEventEndpoint(t *testing.T) {
	var stderr bytes.Buffer
	var eventRequests int
	var finishRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			_, _ = w.Write([]byte(`{"run":{"id":"run_123","leaseID":"","owner":"peter@example.com","org":"openclaw","provider":"aws","class":"standard","serverType":"t3.small","command":["pnpm","test"],"state":"running","phase":"starting","logBytes":0,"logTruncated":false,"startedAt":"2026-05-02T00:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/run_123/events":
			eventRequests++
			http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/run_123/finish":
			finishRequests++
			_, _ = w.Write([]byte(`{"run":{"id":"run_123","leaseID":"","owner":"peter@example.com","org":"openclaw","provider":"aws","class":"standard","serverType":"t3.small","command":["pnpm","test"],"state":"succeeded","phase":"completed","exitCode":0,"logBytes":0,"logTruncated":false,"startedAt":"2026-05-02T00:00:00Z","finishedAt":"2026-05-02T00:00:01Z"}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	rec := newRunRecorder(context.Background(), client, Config{
		Provider:   "aws",
		Class:      "standard",
		ServerType: "t3.small",
	}, []string{"pnpm", "test"}, "", &stderr, false)
	if rec.runID != "run_123" || rec.finished {
		t.Fatalf("run handle must exist before lease attach or finish: %#v", rec)
	}
	rec.AttachLease("cbx_abcdef123456", "blue-lobster", Config{
		Provider:   "aws",
		Class:      "standard",
		ServerType: "t3.small",
	})
	stdout := rec.StreamWriter("stdout")
	if _, err := stdout.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	stdout.Flush()
	rec.waitForOutputEvents(time.Second)
	rec.Finish(context.Background(), SSHTarget{TargetOS: targetWindows}, 0, time.Second, time.Second, "ok", false, nil, FailureClassification{}, nil)

	if eventRequests != 1 {
		t.Fatalf("event requests=%d, want 1", eventRequests)
	}
	if finishRequests != 1 {
		t.Fatalf("finish requests=%d, want 1", finishRequests)
	}
	if text := stderr.String(); strings.Contains(text, "warning:") || !strings.Contains(text, "recording run run_123") {
		t.Fatalf("stderr=%q", text)
	}
}

func TestRunRecorderRequiresCoordinatorHandleBeforeExecution(t *testing.T) {
	rec := &runRecorder{coord: &CoordinatorClient{}}
	err := rec.requireHandle()
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 7 || !strings.Contains(exitErr.Message, "coordinator run handle") {
		t.Fatalf("requireHandle error=%v", err)
	}
	rec.runID = "run_123"
	if err := rec.requireHandle(); err != nil {
		t.Fatalf("coordinator handle rejected: %v", err)
	}
}

func TestRunRecorderFinishUsesExtendedTimeout(t *testing.T) {
	var deadlineRemaining time.Duration
	client := &CoordinatorClient{
		BaseURL: "https://example.test",
		Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost || req.URL.Path != "/v1/runs/run_123/finish" {
				t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
			}
			deadline, ok := req.Context().Deadline()
			if !ok {
				t.Fatal("finish request missing context deadline")
			}
			deadlineRemaining = time.Until(deadline)
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader(`{"run":{"id":"run_123","leaseID":"","owner":"peter@example.com","org":"openclaw","provider":"aws","class":"standard","serverType":"t3.small","command":["pnpm","test"],"state":"succeeded","phase":"completed","exitCode":0,"logBytes":0,"logTruncated":false,"startedAt":"2026-05-02T00:00:00Z","finishedAt":"2026-05-02T00:00:01Z"}}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})},
	}
	rec := &runRecorder{coord: client, runID: "run_123", stderr: io.Discard}
	rec.Finish(context.Background(), SSHTarget{}, 0, time.Second, time.Second, strings.Repeat("x", 2*runLogFallbackPreviewBytes), true, nil, FailureClassification{}, nil)
	if deadlineRemaining < runRecorderFinishTimeout-5*time.Second {
		t.Fatalf("deadline remaining=%s, want near %s", deadlineRemaining, runRecorderFinishTimeout)
	}
}

func TestRunRecorderFinishRetriesIdenticalCommit(t *testing.T) {
	var attempts int
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runs/run_123/finish" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		attempts++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
		if attempts == 1 {
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"run":{"id":"run_123"}}`))
	}))
	defer server.Close()

	rec := &runRecorder{
		coord:  &CoordinatorClient{BaseURL: server.URL, Client: server.Client()},
		runID:  "run_123",
		stderr: io.Discard,
	}
	if err := rec.Finish(context.Background(), SSHTarget{}, 7, time.Second, 2*time.Second, "failed\n", false, nil, FailureClassification{}, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if attempts != 2 || !rec.finished {
		t.Fatalf("attempts=%d finished=%v", attempts, rec.finished)
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("finish retries changed payload: %q %q", bodies[0], bodies[1])
	}
}

func runRecorderTestReceipt(t *testing.T) terminalRunReceipt {
	t.Helper()
	setAttestTestHome(t)
	startedAt := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	receipt, err := buildTerminalRunReceipt("", terminalRunReceiptInput{
		Provider:          "aws",
		RunID:             "run_123",
		Command:           []string{"false"},
		CommandDisplay:    "false",
		ExitCode:          1,
		CommandMs:         100,
		StartedAt:         startedAt,
		EndedAt:           startedAt.Add(100 * time.Millisecond),
		LogSHA256:         sha256Digest(nil),
		RetainedLogSHA256: sha256Digest(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestRunRecorderFinishVerifiesPersistedReceiptAfterSuccess(t *testing.T) {
	receipt := runRecorderTestReceipt(t)
	var finishRequests, receiptRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/run_123/finish":
			finishRequests++
			_, _ = w.Write([]byte(`{"run":{"id":"run_123"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/run_123/receipt":
			receiptRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"receipt": receipt})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	rec := &runRecorder{
		coord:  &CoordinatorClient{BaseURL: server.URL, Client: server.Client()},
		runID:  "run_123",
		stderr: io.Discard,
	}
	if err := rec.Finish(context.Background(), SSHTarget{}, 1, 0, 100*time.Millisecond, "", false, nil, FailureClassification{}, &receipt); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if finishRequests != 1 || receiptRequests != 1 || !rec.finished {
		t.Fatalf("finish=%d receipt=%d finished=%v", finishRequests, receiptRequests, rec.finished)
	}
}

func TestRunRecorderFinishRejectsLegacySuccessWithoutReceiptPersistence(t *testing.T) {
	receipt := runRecorderTestReceipt(t)
	var finishRequests, receiptRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/run_123/finish":
			finishRequests++
			_, _ = w.Write([]byte(`{"run":{"id":"run_123"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/run_123/receipt":
			receiptRequests++
			http.NotFound(w, r)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	rec := &runRecorder{
		coord:  &CoordinatorClient{BaseURL: server.URL, Client: server.Client()},
		runID:  "run_123",
		stderr: io.Discard,
	}
	err := rec.Finish(context.Background(), SSHTarget{}, 1, 0, 100*time.Millisecond, "", false, nil, FailureClassification{}, &receipt)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 7 || !strings.Contains(exitErr.Message, "verify persisted terminal receipt") {
		t.Fatalf("Finish error=%v, want fail-closed receipt verification", err)
	}
	if finishRequests != 1 || receiptRequests != 1 || rec.finished {
		t.Fatalf("finish=%d receipt=%d finished=%v", finishRequests, receiptRequests, rec.finished)
	}
}

func TestRunRecorderFinishRecoversCommittedReceiptAfterLostResponse(t *testing.T) {
	receipt := runRecorderTestReceipt(t)
	var finishRequests, receiptRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/run_123/finish":
			finishRequests++
			_, _ = w.Write([]byte(`{`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/run_123/receipt":
			receiptRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"receipt": receipt})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	rec := &runRecorder{
		coord:  &CoordinatorClient{BaseURL: server.URL, Client: server.Client()},
		runID:  "run_123",
		stderr: io.Discard,
	}
	if err := rec.Finish(context.Background(), SSHTarget{}, 1, 0, 100*time.Millisecond, "", false, nil, FailureClassification{}, &receipt); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if finishRequests != 1 || receiptRequests != 1 || !rec.finished {
		t.Fatalf("finish=%d receipt=%d finished=%v", finishRequests, receiptRequests, rec.finished)
	}
}

func TestRunRecorderResetTelemetryForLeaseReplacement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(done)
	}()
	rec := &runRecorder{
		telemetryStart:   &LeaseTelemetry{CapturedAt: "2026-05-02T00:00:00Z"},
		telemetrySamples: []*LeaseTelemetry{{CapturedAt: "2026-05-02T00:00:01Z"}},
		telemetryCancel:  cancel,
		telemetryDone:    done,
	}

	rec.resetTelemetryForLeaseReplacement()

	if rec.telemetryStart != nil || rec.telemetryCancel != nil || rec.telemetryDone != nil || len(rec.telemetrySamples) != 0 {
		t.Fatalf("telemetry not reset: %#v", rec)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("telemetry sampler was not stopped")
	}
}

type blockingRoundTripper struct {
	started chan struct{}
}

func (t blockingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case <-t.started:
	default:
		close(t.started)
	}
	<-req.Context().Done()
	return nil, context.Cause(req.Context())
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type runEventRecordingRoundTripper struct {
	events *[]CoordinatorRunEventInput
}

func (t runEventRecordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || req.URL.Path != "/v1/runs/run_123/events" {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Status:     "404 Not Found",
			Body:       io.NopCloser(strings.NewReader(`{"error":"not_found"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
	var event CoordinatorRunEventInput
	if err := json.NewDecoder(req.Body).Decode(&event); err != nil {
		return nil, err
	}
	*t.events = append(*t.events, event)
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(strings.NewReader(`{"event":{"runID":"run_123","seq":1,"type":"stdout","createdAt":"2026-05-02T00:00:00Z"}}`)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}
