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

func TestRunAdmissionFailureFinalizesOriginalRequestAfterTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		var original map[string]any
		puts, failures := 0, 0
		client := &CoordinatorClient{BaseURL: "https://example.test", Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if req.Method == http.MethodPut {
				puts++
				original = body
				<-req.Context().Done()
				return nil, req.Context().Err()
			}
			if req.Method != http.MethodPost || req.URL.Path != "/v1/runs/"+admissionTestRunID+"/admission-failure" {
				t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
			}
			failures++
			got, _ := json.Marshal(body["admission"])
			want, _ := json.Marshal(original)
			if !bytes.Equal(got, want) {
				t.Fatalf("changed admission: %s != %s", got, want)
			}
			deadline, ok := req.Context().Deadline()
			if !ok || !deadline.Equal(started.Add(runRecorderRequestTimeout+runRecorderFinishTimeout)) {
				t.Fatalf("terminal deadline=%v", deadline)
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"run":{"id":"` + admissionTestRunID + `","state":"failed","phase":"failed","exitCode":7,"command":["true"],"admissionFailedBeforeWork":true,"endedAt":"2026-09-13T00:00:00Z"}}`))}, nil
		})}}
		var log bytes.Buffer
		rec := newRunRecorder(t.Context(), client, Config{Provider: "aws"}, []string{"true"}, "proof", &log, false, admissionTestRunID)
		if rec.runID != "" || !errors.Is(rec.createErr, context.DeadlineExceeded) || time.Since(started) != runRecorderRequestTimeout {
			t.Fatalf("admission result=%v id=%q elapsed=%v", rec.createErr, rec.runID, time.Since(started))
		}
		primary := rec.requireHandle()
		rec.diagnosticConfig.Provider = "gcp"
		if err := rec.UseCoordinator(&CoordinatorClient{BaseURL: client.BaseURL, Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("terminal bookkeeping used replacement coordinator")
			return nil, errors.New("replacement coordinator")
		})}}); err != nil {
			t.Fatal(err)
		}
		rec.Failed(primary)
		rec.Failed(primary)
		if puts != 1 || failures != 1 || !rec.terminalConfirmed || !rec.finished || rec.requireHandle() == nil {
			t.Fatalf("puts=%d failures=%d confirmed=%t finished=%t", puts, failures, rec.terminalConfirmed, rec.finished)
		}
		if ExitCodeForError(primary, 1) != 7 {
			t.Fatal("primary exit changed")
		}
	})
}

func TestRunAdmissionFailureKeepsUncertaintyAndSharedReportingBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		calls := 0
		client := &CoordinatorClient{BaseURL: "https://example.test", Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			deadline, _ := req.Context().Deadline()
			if !deadline.Equal(start.Add(runRecorderFinishTimeout)) {
				t.Error("reporting deadline reset")
			}
			if calls < 3 {
				time.Sleep(20 * time.Second)
			} else {
				<-req.Context().Done()
			}
			return nil, context.DeadlineExceeded
		})}}
		var log bytes.Buffer
		rec := &runRecorder{coord: client, createCoordinator: client, requestedRunID: admissionTestRunID, createBaseURL: client.BaseURL, createConfig: &Config{}, command: []string{"true"}, stderr: &log}
		rec.Failed(Exit(7, "admission timed out"))
		if calls != 3 || time.Since(start) != runRecorderFinishTimeout || rec.finished || rec.terminalConfirmed || rec.runID != "" {
			t.Fatalf("calls=%d elapsed=%v confirmed=%t", calls, time.Since(start), rec.terminalConfirmed)
		}
		if !strings.Contains(log.String(), "finalization unconfirmed") || !strings.Contains(log.String(), admissionTestRunID) {
			t.Fatalf("unresolved identity missing: %s", &log)
		}
	})
}

func TestRunAdmissionFailureDoesNotFinalizeUnattemptedOrAcknowledgedFinish(t *testing.T) {
	for _, terminalAttempted := range []bool{false, true} {
		calls := 0
		client := &CoordinatorClient{BaseURL: "https://example.test", Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected request") })}}
		rec := &runRecorder{coord: client, requestedRunID: admissionTestRunID, stderr: io.Discard, terminalAttempted: terminalAttempted}
		if terminalAttempted {
			rec.runID = admissionTestRunID
			rec.createConfig = &Config{}
		}
		rec.Failed(errors.New("original failure"))
		if calls != 0 || rec.terminalConfirmed || rec.finished {
			t.Fatalf("unexpected finalization calls=%d", calls)
		}
	}
}

func TestRunAdmissionFailureRequiresCompleteTerminalAcknowledgment(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"confirmed", func(map[string]any) {}},
		{"missing end", func(r map[string]any) { delete(r, "endedAt") }},
		{"invalid end", func(r map[string]any) { r["endedAt"] = "unknown" }},
		{"different identity", func(r map[string]any) { r["id"] = "run_other" }},
		{"no prework marker", func(r map[string]any) { delete(r, "admissionFailedBeforeWork") }},
		{"different exit", func(r map[string]any) { r["exitCode"] = 0 }},
		{"work duration", func(r map[string]any) { r["commandMs"] = 1 }},
		{"different command", func(r map[string]any) { r["command"] = []string{"different"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := map[string]any{"id": admissionTestRunID, "state": "failed", "phase": "failed", "exitCode": 7, "commandMs": 0, "command": []string{"true"}, "admissionFailedBeforeWork": true, "endedAt": "2026-09-13T00:00:00Z"}
			test.change(run)
			body, err := json.Marshal(map[string]any{"run": run})
			if err != nil {
				t.Fatal(err)
			}
			client := &CoordinatorClient{BaseURL: "https://example.test", Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}, nil
			})}}
			_, err = client.FailRunAdmission(t.Context(), admissionTestRunID, "", Config{}, []string{"true"}, "", 7, "admission failed")
			if (err == nil) != (test.name == "confirmed") {
				t.Fatalf("acknowledgment error=%v", err)
			}
		})
	}
}

func TestRunAdmissionFailureRetainsEffectiveAuthenticationAndRedactsWarning(t *testing.T) {
	t.Setenv("CRABBOX_TOKEN_HELPER", "1")
	t.Setenv("CRABBOX_TOKEN_HELPER_VALUE", "first-token")
	t.Setenv("CRABBOX_OWNER", "alice@example.com")
	t.Setenv("CRABBOX_ORG", "example-org")
	puts, failures := 0, 0
	client := &CoordinatorClient{BaseURL: "https://example.test", TokenCommand: synchronousTestHelperCommand("TestCoordinatorTokenCommandHelper")}
	client.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer first-token" || req.Header.Get("X-Crabbox-Owner") != "alice@example.com" || req.Header.Get("X-Crabbox-Org") != "example-org" {
			t.Fatal("original effective admission identity changed")
		}
		if req.Method == http.MethodPut {
			puts++
			t.Setenv("CRABBOX_TOKEN_HELPER_VALUE", "second-token")
			t.Setenv("CRABBOX_OWNER", "bob@example.com")
			t.Setenv("CRABBOX_ORG", "other-org")
			return &http.Response{StatusCode: http.StatusInternalServerError, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("first-token configured-diagnostic-marker"))}, nil
		}
		failures++
		return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("first-token configured-diagnostic-marker"))}, nil
	})}
	var log bytes.Buffer
	rec := newRunRecorder(t.Context(), client, Config{CoordToken: "configured-diagnostic-marker"}, []string{"true"}, "", &log, false, admissionTestRunID)
	primary := rec.requireHandle()
	attachErr := rec.AttachLease(t.Context(), "cbx_test", "", Config{})
	for _, presented := range []error{primary, attachErr} {
		var exitErr ExitError
		if !AsExitError(presented, &exitErr) || exitErr.Code != 7 {
			t.Fatal("admission presentation changed its exit classification")
		}
		if strings.Contains(exitErr.Message, "first-token") || strings.Contains(exitErr.Message, "configured-diagnostic-marker") {
			t.Fatal("admission error presentation exposed a known diagnostic value")
		}
		if !strings.Contains(exitErr.Message, "500") || !strings.Contains(exitErr.Message, admissionTestRunID) || !strings.Contains(exitErr.Message, "unavailable before command") {
			t.Fatal("admission error presentation lost status, identity or useful context")
		}
	}
	var stored CoordinatorHTTPError
	if !errors.As(rec.createErr, &stored) || stored.StatusCode != 500 || !strings.Contains(stored.Message, "first-token") {
		t.Fatal("presentation redaction changed the stored admission cause")
	}
	rec.Failed(primary)
	if puts != 4 || failures != 1 || rec.terminalConfirmed || ExitCodeForError(primary, 1) != 7 {
		t.Fatalf("unexpected outcome: puts=%d failures=%d confirmed=%t", puts, failures, rec.terminalConfirmed)
	}
	if strings.Contains(log.String(), "first-token") || strings.Contains(log.String(), "configured-diagnostic-marker") {
		t.Fatal("terminal warning exposed a known diagnostic value")
	}
	if !strings.Contains(log.String(), "403") || !strings.Contains(log.String(), admissionTestRunID) || !strings.Contains(log.String(), "finalization unconfirmed") {
		t.Fatal("terminal warning lost status, identity or uncertainty")
	}
}

func TestRunAdmissionAuthenticationCaptureUsesOriginalBudget(t *testing.T) {
	t.Setenv("CRABBOX_TOKEN_HELPER", "1")
	t.Setenv("CRABBOX_TOKEN_HELPER_VALUE", "first-token")
	t.Setenv("CRABBOX_TOKEN_HELPER_DELAY", "30s")
	calls := 0
	client := &CoordinatorClient{BaseURL: "https://example.test", TokenCommand: synchronousTestHelperCommand("TestCoordinatorTokenCommandHelper"), Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected dispatch")
	})}}
	started := time.Now()
	var log bytes.Buffer
	rec := newRunRecorder(t.Context(), client, Config{}, []string{"true"}, "", &log, false, admissionTestRunID)
	if elapsed := time.Since(started); elapsed < runRecorderRequestTimeout || elapsed > runRecorderRequestTimeout+5*time.Second {
		t.Fatalf("authentication capture did not use the original admission budget: %v", elapsed)
	}
	if !errors.Is(rec.createErr, context.DeadlineExceeded) {
		t.Fatalf("original admission deadline cause lost: %v", rec.createErr)
	}
	t.Setenv("CRABBOX_TOKEN_HELPER_DELAY", "0s")
	rec.Failed(rec.requireHandle())
	if calls != 0 || rec.terminalConfirmed || rec.createCoordinator.admissionAuth.err == nil {
		t.Fatal("failed initial authentication was recaptured or dispatched during bookkeeping")
	}
}
