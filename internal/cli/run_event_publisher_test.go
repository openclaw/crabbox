package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	go func() { attached <- rec.AttachLease(t.Context(), "cbx_new", "new-slug", Config{}) }()
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
		if req.URL.Path == "/v1/runs/"+admissionTestRunID {
			io.WriteString(w, `{"run":{"id":"run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","leaseID":"cbx_existing","slug":"existing","provider":"aws","state":"running","phase":"starting","command":["true"]}}`)
			return
		}
		close(started)
		<-release
		io.WriteString(w, `{"event":{"type":"lease.created"}}`)
	}))
	defer server.Close()
	cfg := Config{Provider: "aws"}
	rec := newRunRecorder(context.Background(), &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}, cfg, []string{"true"}, "", io.Discard, true, admissionTestRunID)
	attached := make(chan error, 1)
	go func() { attached <- rec.AttachLease(t.Context(), "cbx_existing", "existing", cfg) }()
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

func TestRunRecorderBindingIgnoresDiagnosticBacklog(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprint("full=", full), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := make(chan struct{})
				diagnosticDone := false
				client := &CoordinatorClient{BaseURL: "https://example.test", Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					var input CoordinatorRunEventInput
					json.NewDecoder(req.Body).Decode(&input)
					if input.Type == "command.started" {
						close(started)
						select {
						case <-time.After(8 * time.Second):
						case <-req.Context().Done():
							time.Sleep(time.Second)
						}
						diagnosticDone = true
						if err := req.Context().Err(); err != nil {
							return nil, err
						}
					}
					if input.Type == "lease.created" {
						if !diagnosticDone {
							t.Error("binding overtook diagnostic publisher join")
						}
						deadline, ok := req.Context().Deadline()
						if !ok || deadline.Sub(time.Now()) != runRecorderRequestTimeout {
							t.Error("binding did not receive its own HTTP request budget")
						}
						select {
						case <-time.After(3 * time.Second):
						case <-req.Context().Done():
							return nil, req.Context().Err()
						}
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"event":{"type":"accepted"}}`)), Header: make(http.Header), Request: req}, nil
				})}}
				rec := &runRecorder{coord: client, runID: "run_123", stderr: io.Discard}
				rec.Event("command.started", "command", "")
				<-started
				if full {
					for range runEventOutputQueueSize {
						rec.Event("sync.finished", "synced", "")
					}
				}
				if err := rec.AttachLease(t.Context(), "cbx_replacement", "replacement", Config{Provider: "aws"}); err != nil {
					t.Errorf("healthy binding rejected by diagnostic backlog: %v", err)
				}
				rec.Failed(nil)
			})
		})
	}
}

func TestRunRecorderMissingBindingEndpointFailsAdmission(t *testing.T) {
	for _, oldLease := range []string{"", "cbx_initial"} {
		t.Run("old="+oldLease, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
			}))
			defer server.Close()
			rec := &runRecorder{coord: &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}, runID: "run_123", leaseID: oldLease, stderr: io.Discard}
			err := rec.AttachLease(t.Context(), "cbx_next", "next", Config{Provider: "aws"})
			if ExitCodeForError(err, 0) != 7 || !strings.Contains(err.Error(), "lease attribution") {
				t.Fatalf("unbound run was admitted without a valid binding: %v", err)
			}
			if calls != 1 || rec.leaseID != oldLease {
				t.Fatalf("failed binding changed attribution: calls=%d lease=%q", calls, rec.leaseID)
			}
			rec.Failed(nil)
		})
	}
}

func TestRunEventAppendRecovery(t *testing.T) {
	for _, scenario := range []string{"slow", "503", "lost acknowledgement", "timeout", "permanent", "legacy", "exhausted", "budget", "unsupported", "429"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var requests []map[string]any
				client := &CoordinatorClient{BaseURL: "https://example.test", Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					var input map[string]any
					if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
						t.Fatal(err)
					}
					requests = append(requests, input)
					wantMethod := http.MethodPut
					if scenario == "legacy" {
						wantMethod = http.MethodPost
					}
					if req.Method != wantMethod {
						t.Errorf("method=%s, want %s", req.Method, wantMethod)
					}
					if scenario == "budget" {
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					if len(requests) == 1 {
						switch scenario {
						case "slow":
							select {
							case <-time.After(3 * time.Second):
							case <-req.Context().Done():
								return nil, req.Context().Err()
							}
						case "timeout":
							<-req.Context().Done()
							return nil, req.Context().Err()
						case "lost acknowledgement":
							return nil, io.ErrUnexpectedEOF
						}
					}
					code := 201
					if scenario == "unsupported" {
						code = 404
					}
					if scenario == "429" && len(requests) == 1 {
						code = 429
					}
					if scenario == "exhausted" || ((scenario == "503" || scenario == "legacy") && len(requests) == 1) {
						code = 503
					}
					if scenario == "permanent" {
						code = 403
					}
					return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(`{"event":{"seq":2}}`)), Header: make(http.Header), Request: req}, nil
				})}}
				var run CoordinatorRun
				json.Unmarshal([]byte(fmt.Sprintf(`{"id":"run_123","eventAppendIdempotent":%t}`, scenario != "legacy")), &run)
				var diagnostics strings.Builder
				rec := &runRecorder{coord: client, stderr: &diagnostics}
				rec.attachRun(run)
				startedAt := time.Now()
				rec.publisher.Enqueue(client, rec.runID, "stdout", "hello")
				rec.waitForEvents(40 * time.Second)
				want := 2
				if scenario == "slow" || scenario == "permanent" || scenario == "legacy" || scenario == "unsupported" {
					want = 1
				}
				if scenario == "exhausted" || scenario == "budget" {
					want = 3
				}
				if len(requests) != want {
					t.Fatalf("requests=%d, want %d; %s", len(requests), want, diagnostics.String())
				}
				if scenario != "legacy" {
					if id, ok := requests[0]["id"].(string); !ok || len(id) != 32 {
						t.Fatalf("missing stable event ID: %v", requests[0])
					}
					for _, input := range requests[1:] {
						if !reflect.DeepEqual(input, requests[0]) {
							t.Fatal("retry changed append payload")
						}
					}
				}
				if scenario == "budget" && time.Since(startedAt) != 30*time.Second {
					t.Fatalf("retry budget=%v", time.Since(startedAt))
				}
				failed := scenario == "permanent" || scenario == "legacy" || scenario == "exhausted" || scenario == "budget"
				if strings.Contains(diagnostics.String(), "append failed") != failed {
					t.Fatalf("unexpected diagnostics: %s", diagnostics.String())
				}
			})
		})
	}
}

func TestRunRecorderIdempotentDrainPreservesEventOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var observed []string
		var ids []string
		started := make(chan struct{})
		var diagnostics strings.Builder
		client := &CoordinatorClient{BaseURL: "https://example.test", Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			kind := "finish"
			if strings.HasSuffix(req.URL.Path, "/events") {
				var input CoordinatorRunEventInput
				if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
					t.Fatal(err)
				}
				kind = input.Type
				ids = append(ids, input.ID)
			}
			observed = append(observed, req.Method+":"+kind)
			code := http.StatusOK
			if len(observed) == 1 {
				close(started)
				select {
				case <-time.After(3 * time.Second):
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
				code = http.StatusServiceUnavailable
			}
			return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(`{"run":{"id":"run_123"},"event":{"seq":2}}`)), Header: make(http.Header), Request: req}, nil
		})}}
		rec := &runRecorder{coord: client, stderr: &diagnostics}
		rec.attachRun(CoordinatorRun{ID: "run_123", EventAppendIdempotent: true})
		rec.publisher.Enqueue(client, rec.runID, "stdout", "hello")
		<-started
		rec.Event("command.finished", "finished", "")
		if err := rec.Finish(context.Background(), SSHTarget{}, 0, 0, 0, "hello", false, nil, FailureClassification{}, nil); err != nil {
			t.Fatal(err)
		}
		want := []string{"PUT:stdout", "PUT:stdout", "PUT:command.finished", "POST:finish"}
		if !reflect.DeepEqual(observed, want) {
			t.Fatalf("publication order=%v, want %v", observed, want)
		}
		if ids[0] == "" || ids[0] != ids[1] || ids[1] == ids[2] {
			t.Fatalf("wrong idempotency keys: %v", ids)
		}
		if strings.Contains(diagnostics.String(), "warning:") {
			t.Fatalf("recovered append warned: %s", diagnostics.String())
		}
	})
}
