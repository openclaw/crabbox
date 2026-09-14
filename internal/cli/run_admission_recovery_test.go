package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

const admissionTestRunID = "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func admissionTestResponse(w http.ResponseWriter, req *http.Request, state, phase string) {
	json.NewEncoder(w).Encode(CoordinatorRunResponse{Run: CoordinatorRun{ID: admissionTestRunID, State: state, Phase: phase, Command: []string{"true"}, LeaseID: "cbx_test"}})
}

func TestRunAdmissionUsesReplaySafeRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPut || req.URL.Path != "/v1/runs/"+admissionTestRunID {
			http.NotFound(w, req)
			return
		}
		admissionTestResponse(w, req, "running", "starting")
	}))
	defer server.Close()
	client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	rec := newRunRecorder(t.Context(), client, Config{}, []string{"true"}, "", io.Discard, true, admissionTestRunID)
	if err := rec.AttachLease(t.Context(), "cbx_test", "", Config{}); err != nil {
		t.Fatal(err)
	}
	if err := rec.requireHandle(); err != nil {
		t.Fatalf("replay-safe admission unavailable: %v", err)
	}
}

func TestRunAdmissionRejectsTerminalAndAdvancedRecords(t *testing.T) {
	for _, record := range []struct{ state, phase string }{{"succeeded", "completed"}, {"failed", "failed"}, {"running", "command"}} {
		t.Run(record.state+"-"+record.phase, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				admissionTestResponse(w, req, record.state, record.phase)
			}))
			defer server.Close()
			rec := newRunRecorder(t.Context(), &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}, Config{}, []string{"true"}, "", io.Discard, true, admissionTestRunID)
			_ = rec.AttachLease(t.Context(), "cbx_test", "", Config{})
			if rec.requireHandle() == nil {
				t.Fatal("historical or advanced record authorized fresh execution")
			}
		})
	}
}

func TestRunAdmissionRetainsInvocationCancellation(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls++
		admissionTestResponse(w, req, "running", "starting")
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	rec := newRunRecorder(ctx, &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}, Config{}, []string{"true"}, "", io.Discard, true, admissionTestRunID)
	cancel()
	_ = rec.AttachLease(ctx, "cbx_test", "", Config{})
	if calls != 0 || rec.requireHandle() == nil {
		t.Fatalf("canceled invocation admitted: calls=%d idPresent=%t", calls, strings.TrimSpace(rec.runID) != "")
	}
}

func TestRunAdmissionRecoversCommittedResponseWithoutChangingBinding(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	committed := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/events") {
			io.WriteString(w, `{"event":{"type":"lease.created"}}`)
			return
		}
		if req.Method != http.MethodPut || req.URL.Path != "/v1/runs/"+admissionTestRunID {
			t.Errorf("unexpected admission route %s %s", req.Method, req.URL.Path)
			http.NotFound(w, req)
			return
		}
		body, _ := io.ReadAll(req.Body)
		mu.Lock()
		bodies = append(bodies, body)
		first := len(bodies) == 1
		if first {
			committed++
		}
		mu.Unlock()
		if first {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		json.NewEncoder(w).Encode(CoordinatorRunResponse{Run: CoordinatorRun{ID: admissionTestRunID, State: "running", Phase: "starting", Command: []string{"true"}}})
	}))
	defer server.Close()
	original := []string{"true"}
	rec := newRunRecorder(t.Context(), &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}, Config{Provider: "aws"}, original, "label", io.Discard, false, admissionTestRunID)
	defer rec.Failed(nil)
	original[0] = "changed"
	if err := rec.AttachLease(t.Context(), "cbx_replacement", "replacement", Config{Provider: "gcp"}); err != nil {
		t.Fatal(err)
	}
	if err := rec.requireHandle(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if committed != 1 || len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("admission replay changed binding: commits=%d requests=%d", committed, len(bodies))
	}
	var body map[string]any
	json.Unmarshal(bodies[0], &body)
	if body["leaseID"] != "" || body["provider"] != "aws" || rec.runID != admissionTestRunID || rec.leaseID != "cbx_replacement" {
		t.Fatalf("immutable create or actual attribution lost: request=%v lease=%s", body, rec.leaseID)
	}
}

func TestRunAdmissionRecoverySharesTenSecondBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		calls := 0
		client := &CoordinatorClient{BaseURL: "https://example.test", Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				time.Sleep(7 * time.Second)
				return nil, io.ErrUnexpectedEOF
			}
			deadline, ok := req.Context().Deadline()
			if !ok || !deadline.Equal(started.Add(10*time.Second)) {
				t.Error("recovery reset admission budget")
			}
			<-req.Context().Done()
			return nil, req.Context().Err()
		})}}
		_, err := client.CreateRun(t.Context(), admissionTestRunID, "cbx_test", Config{}, []string{"true"}, "")
		if !errors.Is(err, context.DeadlineExceeded) || calls != 2 || time.Since(started) != 10*time.Second {
			t.Fatalf("budget result calls=%d elapsed=%s err=%v", calls, time.Since(started), err)
		}
	})
}

func TestRunAdmissionRejectsMismatchedRepliesWithoutRetry(t *testing.T) {
	for _, mismatch := range []string{"identity", "command"} {
		t.Run(mismatch, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				run := CoordinatorRun{ID: admissionTestRunID, State: "running", Phase: "starting", Command: []string{"true"}}
				if mismatch == "identity" {
					run.ID = "run_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				} else {
					run.Command = []string{"other"}
				}
				json.NewEncoder(w).Encode(CoordinatorRunResponse{Run: run})
			}))
			defer server.Close()
			rec := newRunRecorder(t.Context(), &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}, Config{}, []string{"true"}, "", io.Discard, false, admissionTestRunID)
			err := rec.AttachLease(t.Context(), "cbx_test", "", Config{})
			if calls != 1 || rec.createPending || rec.runID != "" || err == nil || !strings.Contains(err.Error(), "mismatched") {
				t.Fatalf("mismatched reply recovered: calls=%d pending=%v err=%v", calls, rec.createPending, err)
			}
		})
	}
}

func TestRunAdmissionRejectsResponseAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &CoordinatorClient{BaseURL: "https://example.test", Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		cancel()
		body, _ := json.Marshal(CoordinatorRunResponse{Run: CoordinatorRun{ID: admissionTestRunID, State: "running", Phase: "starting", Command: []string{"true"}}})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}}
	rec := newRunRecorder(ctx, client, Config{}, []string{"true"}, "", io.Discard, true, admissionTestRunID)
	if rec.AttachLease(ctx, "cbx_test", "", Config{}) == nil || rec.runID != "" || rec.createPending {
		t.Fatal("late response admitted canceled invocation")
	}
}

func TestRunAdmissionCannotMoveToAnotherCoordinator(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { admissionTestResponse(w, req, "running", "starting") }))
	defer server.Close()
	original := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	rec := newRunRecorder(t.Context(), original, Config{}, []string{"true"}, "", io.Discard, false, admissionTestRunID)
	if err := rec.requireHandle(); err != nil {
		t.Fatal(err)
	}
	if err := rec.UseCoordinator(&CoordinatorClient{BaseURL: "https://replacement.invalid"}); err == nil {
		t.Fatal("admission silently changed coordinator")
	}
	if rec.coord != original {
		t.Fatal("refusal replaced the original coordinator")
	}
	refreshed := &CoordinatorClient{BaseURL: server.URL, Token: "refreshed-fixture", Client: server.Client()}
	if err := rec.UseCoordinator(refreshed); err != nil {
		t.Fatal(err)
	}
}

func TestRunAdmissionCannotReuseFieldsFromFailedDecode(t *testing.T) {
	calls := 0
	client := &CoordinatorClient{BaseURL: "https://example.test", Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body := `{"run":{"id":"` + admissionTestRunID + `","state":"running","phase":"starting"}}`
		if calls == 1 {
			body = `{"run":{"id":"` + admissionTestRunID + `","state":"running","phase":"starting","command":["true"],"eventCount":"malformed"}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}
	_, err := client.CreateRun(t.Context(), admissionTestRunID, "cbx_test", Config{}, []string{"true"}, "")
	if err == nil || calls != 2 {
		t.Fatalf("failed response supplied later admission fields: calls=%d err=%v", calls, err)
	}
}
