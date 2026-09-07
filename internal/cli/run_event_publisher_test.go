package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestRunRecorderPhaseDoesNotDelayWorkload(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(started)
		<-release
		io.WriteString(w, `{"event":{"type":"command.started"}}`)
	}))
	defer server.Close()
	rec := &runRecorder{coord: &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}, runID: "run_123", stderr: io.Discard}
	returned := make(chan struct{})
	go func() { rec.Event("command.started", "command", "printf hello"); close(returned) }()
	<-started
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Error("workload admission waited for best-effort phase publication")
	}
	close(release)
	<-returned
	rec.waitForEvents(time.Second)
}

func TestRunRecorderEventPublisherJoinsBeforeTerminal(t *testing.T) {
	for _, action := range []string{"finish", "failed", "early return"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				eventStarted := make(chan struct{})
				canceled := make(chan struct{})
				release := make(chan struct{})
				finished := make(chan struct{})
				var stderr strings.Builder
				client := &CoordinatorClient{BaseURL: "https://example.test", Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					var input CoordinatorRunEventInput
					if strings.HasSuffix(req.URL.Path, "/events") {
						json.NewDecoder(req.Body).Decode(&input)
					}
					if input.Type == "stdout" {
						close(eventStarted)
						<-req.Context().Done()
						close(canceled)
						<-release
						return nil, req.Context().Err()
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"run":{"id":"run_123"},"event":{"type":"run.failed"}}`)), Header: make(http.Header), Request: req}, nil
				})}}
				rec := &runRecorder{coord: client, runID: "run_123", stderr: &stderr}
				writer := rec.StreamWriter("stdout")
				writer.Write([]byte("hello"))
				writer.Flush()
				<-eventStarted
				go func() {
					if action == "finish" {
						rec.Finish(context.Background(), SSHTarget{}, 0, 0, 0, "hello", false, nil, FailureClassification{}, nil)
					} else if action == "failed" {
						rec.Failed(errors.New("setup failed"))
					} else {
						rec.Failed(nil)
					}
					close(finished)
				}()
				<-canceled
				time.Sleep(2 * time.Second)
				select {
				case <-finished:
					t.Error("terminal operation returned while its event publisher was still running")
				default:
				}
				close(release)
				<-finished
				if !strings.Contains(stderr.String(), "diagnostics dropped") {
					t.Errorf("lost diagnostics have no visible outcome: %s", stderr.String())
				}
				rec.waitForEvents(time.Second)
			})
		})
	}
}

func TestRunRecorderOrdersEventsAcrossLeaseReplacementAndFinish(t *testing.T) {
	var mu sync.Mutex
	var observed []string
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var input CoordinatorRunEventInput
		if strings.HasSuffix(req.URL.Path, "/events") {
			json.NewDecoder(req.Body).Decode(&input)
		}
		if input.Type == "lease.replace.started" {
			close(started)
			<-release
		}
		kind := input.Type
		if kind == "" {
			kind = "finish"
		}
		mu.Lock()
		observed = append(observed, kind+":"+req.Header.Get("Authorization"))
		mu.Unlock()
		io.WriteString(w, `{"run":{"id":"run_123"},"event":{"type":"accepted"}}`)
	}))
	defer server.Close()
	rec := &runRecorder{coord: &CoordinatorClient{BaseURL: server.URL, Token: "old", Client: server.Client()}, runID: "run_123", leaseID: "cbx_old", stderr: io.Discard}
	writer := rec.StreamWriter("stdout")
	rec.Event("lease.replace.started", "leasing", "")
	<-started
	rec.UseCoordinator(&CoordinatorClient{BaseURL: server.URL, Token: "new", Client: server.Client()})
	attached := make(chan error, 1)
	go func() { attached <- rec.AttachLease("cbx_new", "new-slug", Config{}) }()
	select {
	case <-attached:
		t.Error("replacement admitted before its authoritative lease attachment")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-attached; err != nil {
		t.Fatal(err)
	}
	rec.Event("command.started", "command", "")
	writer.Write([]byte("hello"))
	writer.Flush()
	if err := rec.Finish(context.Background(), SSHTarget{}, 0, 0, 0, "hello", false, nil, FailureClassification{}, nil); err != nil {
		t.Fatal(err)
	}
	rec.Event("lease.released", "released", "")
	mu.Lock()
	defer mu.Unlock()
	want := []string{"lease.replace.started:Bearer old", "lease.created:Bearer new", "command.started:Bearer new", "stdout:Bearer new", "finish:Bearer new", "lease.released:Bearer new"}
	if !reflect.DeepEqual(observed, want) {
		t.Fatalf("event order/coordinator=%v, want %v", observed, want)
	}
}

func TestRunRecorderExistingLeaseCreateDoesNotWaitForDiagnostic(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/runs" {
			io.WriteString(w, `{"run":{"id":"run_123","leaseID":"cbx_existing","slug":"existing","provider":"aws"}}`)
			return
		}
		close(started)
		<-release
		io.WriteString(w, `{"event":{"type":"lease.created"}}`)
	}))
	defer server.Close()
	cfg := Config{Provider: "aws"}
	rec := newRunRecorder(context.Background(), &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}, cfg, []string{"true"}, "", io.Discard, true)
	attached := make(chan error, 1)
	go func() { attached <- rec.AttachLease("cbx_existing", "existing", cfg) }()
	<-started
	select {
	case err := <-attached:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("authoritative CreateRun binding waited for diagnostic publication")
	}
	close(release)
	rec.waitForEvents(time.Second)
	if err := rec.requireHandle(); err != nil {
		t.Fatal(err)
	}
}
