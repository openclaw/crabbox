package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"nhooyr.io/websocket"
)

func TestHistoryJSONPreservesLeaseAttribution(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		_, _ = w.Write([]byte(`{"runs":[{"id":"run_123","leaseID":"cbx_2","leaseIDs":["cbx_1","cbx_2"],"owner":"bob@example.com","org":"elsewhere","leaseOwners":[{"owner":"alice@example.com","org":"example-org"},{"owner":"bob@example.com","org":"elsewhere"}],"provider":"aws","class":"standard","serverType":"t3.small","command":["true"],"state":"succeeded","logBytes":0,"logTruncated":false,"startedAt":"2026-05-02T00:00:00Z"}]}`))
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.history(context.Background(), []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var runs []CoordinatorRun
	if err := json.Unmarshal(stdout.Bytes(), &runs); err != nil {
		t.Fatalf("decode history JSON: %v", err)
	}
	if len(runs) != 1 || len(runs[0].LeaseIDs) != 2 || runs[0].LeaseIDs[0] != "cbx_1" {
		t.Fatalf("lease IDs not preserved: %#v", runs)
	}
	if len(runs[0].LeaseOwners) != 2 || runs[0].LeaseOwners[0].Owner != "alice@example.com" {
		t.Fatalf("lease owners not preserved: %#v", runs[0].LeaseOwners)
	}
}

func TestEventsCommandPassesPagination(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.String()
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs/run_123/events" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"events":[{"runID":"run_123","seq":5,"type":"sync.finished","phase":"synced","createdAt":"2026-05-02T00:00:00Z"}]}`))
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.events(context.Background(), []string{"run_123", "--after", "4", "--limit", "25"}); err != nil {
		t.Fatal(err)
	}
	if path != "/v1/runs/run_123/events?after=4&limit=25" {
		t.Fatalf("path=%q", path)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("sync.finished")) {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestAttachCommandReplaysOutputAndStopsWhenRunFinished(t *testing.T) {
	eventCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/control":
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/run_123/events":
			eventCalls++
			if eventCalls == 1 {
				if got := r.URL.Query().Get("after"); got != "" {
					t.Fatalf("first after=%q", got)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{
					{"runID": "run_123", "seq": 1, "type": "stdout", "stream": "stdout", "data": "hello\n", "createdAt": "2026-05-02T00:00:00Z"},
					{"runID": "run_123", "seq": 2, "type": "stderr", "stream": "stderr", "data": "warn\n", "createdAt": "2026-05-02T00:00:01Z"},
				}})
				return
			}
			if got := r.URL.Query().Get("after"); got != "2" {
				t.Fatalf("next after=%q", got)
			}
			_, _ = w.Write([]byte(`{"events":[]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/run_123":
			_, _ = w.Write([]byte(`{"run":{"id":"run_123","leaseID":"cbx_123","owner":"peter@example.com","org":"openclaw","provider":"aws","class":"standard","serverType":"t3.small","command":["true"],"state":"succeeded","phase":"finished","logBytes":0,"logTruncated":false,"startedAt":"2026-05-02T00:00:00Z"}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.attach(context.Background(), []string{"run_123", "--poll", "1ms"}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "hello\n" {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if stderr.String() != "warn\n" {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if eventCalls != 2 {
		t.Fatalf("eventCalls=%d, want 2", eventCalls)
	}
}

func TestAttachCommandStreamsOverControlWebSocket(t *testing.T) {
	controlCalls := 0
	eventCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/control":
			controlCalls++
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept control websocket: %v", err)
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "")
			for {
				_, data, err := conn.Read(r.Context())
				if err != nil {
					return
				}
				var msg map[string]any
				if err := json.Unmarshal(data, &msg); err != nil {
					t.Errorf("control message JSON: %v", err)
					return
				}
				if msg["type"] == "subscribe_run" {
					if msg["runID"] != "run_123" {
						t.Errorf("runID=%v", msg["runID"])
					}
					_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"run_events","runID":"run_123","events":[{"runID":"run_123","seq":1,"type":"stdout","stream":"stdout","data":"hello ws\n","createdAt":"2026-05-02T00:00:00Z"}],"nextSeq":1}`))
				}
			}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/run_123":
			_, _ = w.Write([]byte(`{"run":{"id":"run_123","leaseID":"cbx_123","owner":"peter@example.com","org":"openclaw","provider":"aws","class":"standard","serverType":"t3.small","command":["true"],"state":"succeeded","phase":"finished","logBytes":0,"logTruncated":false,"startedAt":"2026-05-02T00:00:00Z"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/run_123/events":
			eventCalls++
			_, _ = w.Write([]byte(`{"events":[]}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.attach(context.Background(), []string{"run_123", "--poll", "50ms"}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "hello ws\n" {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if controlCalls != 1 {
		t.Fatalf("controlCalls=%d, want 1", controlCalls)
	}
	if eventCalls != 0 {
		t.Fatalf("HTTP eventCalls=%d, want websocket attach to avoid polling events", eventCalls)
	}
}

func TestAttachCommandDrainsControlWebSocketBacklogPages(t *testing.T) {
	controlSubscribes := 0
	eventCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/control":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept control websocket: %v", err)
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "")
			for {
				_, data, err := conn.Read(r.Context())
				if err != nil {
					return
				}
				var msg map[string]any
				if err := json.Unmarshal(data, &msg); err != nil {
					t.Errorf("control message JSON: %v", err)
					return
				}
				if msg["type"] != "subscribe_run" {
					continue
				}
				controlSubscribes++
				after := int(msg["after"].(float64))
				events := make([]map[string]any, 0, coordinatorControlRunEventLimit)
				start, end := after+1, after+coordinatorControlRunEventLimit
				if after >= coordinatorControlRunEventLimit {
					end = after + 1
				}
				for seq := start; seq <= end; seq++ {
					events = append(events, map[string]any{
						"runID":     "run_123",
						"seq":       seq,
						"type":      "stdout",
						"stream":    "stdout",
						"data":      fmt.Sprintf("line %03d\n", seq),
						"createdAt": "2026-05-02T00:00:00Z",
					})
				}
				if err := conn.Write(r.Context(), websocket.MessageText, mustJSON(t, map[string]any{
					"type":    "run_events",
					"runID":   "run_123",
					"events":  events,
					"nextSeq": events[len(events)-1]["seq"],
				})); err != nil {
					t.Errorf("write control events: %v", err)
					return
				}
			}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/run_123":
			_, _ = w.Write([]byte(`{"run":{"id":"run_123","leaseID":"cbx_123","owner":"peter@example.com","org":"openclaw","provider":"aws","class":"standard","serverType":"t3.small","command":["true"],"state":"succeeded","phase":"finished","logBytes":0,"logTruncated":false,"startedAt":"2026-05-02T00:00:00Z"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/run_123/events":
			eventCalls++
			_, _ = w.Write([]byte(`{"events":[]}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.attach(context.Background(), []string{"run_123", "--poll", "50ms"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("line 001\n")) || !bytes.Contains(stdout.Bytes(), []byte("line 101\n")) {
		t.Fatalf("stdout missing backlog boundary lines: %q", stdout.String())
	}
	if controlSubscribes < 2 {
		t.Fatalf("controlSubscribes=%d, want backlog page follow-up", controlSubscribes)
	}
	if eventCalls != 0 {
		t.Fatalf("HTTP eventCalls=%d, want websocket attach to avoid polling events", eventCalls)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
