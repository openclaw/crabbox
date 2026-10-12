package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestCoordinatorTransientUnavailable(t *testing.T) {
	for _, tc := range []struct {
		status          int
		message, reason string
	}{
		{500, "error code: 1101", "cloudflare_1101"},
		{500, " \nerror code: 1102\n", "cloudflare_1102"},
		{500, `{"error":"x"}`, ""},
		{500, "error code: 1101 extra", ""},
		{429, "busy", "http_429"},
		{502, "busy", "http_502"},
		{503, "busy", "http_503"},
		{504, "busy", "http_504"},
		{400, "error code: 1101", ""},
		{404, "missing", ""},
		{409, "conflict", ""},
	} {
		t.Run(fmt.Sprintf("%d/%s", tc.status, tc.message), func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", CoordinatorHTTPError{StatusCode: tc.status, Message: tc.message})
			if got := coordinatorTransientUnavailableReason(err); got != tc.reason {
				t.Fatalf("shared reason=%q want=%q", got, tc.reason)
			}
			if got := coordinatorReadRetryReason(err); got != tc.reason {
				t.Fatalf("reason=%q want=%q", got, tc.reason)
			}
		})
	}
}

func TestCoordinatorReadCloudflare1101(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "alice@example.test")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "error code: 1101", 500)
			return
		}
		io.WriteString(w, `{"lease":{"id":"cbx_read"}}`)
	}))
	defer server.Close()
	c := &CoordinatorClient{BaseURL: server.URL, Client: server.Client(), readRetryWriter: io.Discard}
	lease, err := c.GetLease(t.Context(), "cbx_read")
	if err != nil || calls.Load() != 2 || lease.ID != "cbx_read" {
		t.Fatalf("lease=%s err=%v calls=%d", lease.ID, err, calls.Load())
	}
}

func TestCoordinatorRunAdmissionBackoffAndBinding(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "alice@example.test")
	for _, tc := range []struct {
		status                 int
		message, after, reason string
	}{
		{503, `{"error":"coordinator_unavailable"}`, "1", "http_503"},
		{500, "error code: 1101", "", "cloudflare_1101"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			var bodies []string
			var headers []http.Header
			var times []time.Time
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				bodies = append(bodies, string(body))
				headers = append(headers, r.Header.Clone())
				times = append(times, time.Now())
				if r.Method != "PUT" || r.URL.Path != "/v1/runs/"+admissionTestRunID {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if len(bodies) == 1 {
					w.Header().Set("Retry-After", tc.after)
					http.Error(w, tc.message, tc.status)
					return
				}
				fmt.Fprintf(w, `{"run":{"id":%q,"state":"running","phase":"starting","command":["true"]}}`, admissionTestRunID)
			}))
			defer server.Close()
			var progress bytes.Buffer
			c := &CoordinatorClient{BaseURL: server.URL, Client: server.Client(), Token: "admission-test", admissionAuth: &coordinatorAdmissionAuth{}, readRetryWriter: &progress}
			c.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				response, err := http.DefaultTransport.RoundTrip(r)
				// A retry must keep the already captured identity even if configured auth changes.
				c.Token = "changed-test"
				return response, err
			})
			run, err := c.CreateRun(t.Context(), admissionTestRunID, "", Config{}, []string{"true"}, "test")
			if err != nil || run.ID != admissionTestRunID || len(bodies) != 2 {
				t.Fatalf("run=%s err=%v requests=%d", run.ID, err, len(bodies))
			}
			if bodies[0] != bodies[1] || !reflect.DeepEqual(headers[0], headers[1]) || headers[1].Get("Authorization") != "Bearer admission-test" {
				t.Fatal("retry changed body or captured admission headers")
			}
			if delay := times[1].Sub(times[0]); delay < time.Second {
				t.Fatalf("retry delay=%s", delay)
			}
			if want := "coordinator run admission retry 1/3 reason=" + tc.reason + "\n"; progress.String() != want {
				t.Fatalf("progress=%q want=%q", progress.String(), want)
			}
		})
	}
}

func TestCoordinatorRunAdmissionStopsBeforeBudgetOrOn400(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "alice@example.test")
	for _, status := range []int{400, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "8")
				http.Error(w, "unavailable", status)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			start := time.Now()
			c := &CoordinatorClient{BaseURL: server.URL, Client: server.Client(), readRetryWriter: io.Discard}
			_, err := c.CreateRun(ctx, admissionTestRunID, "", Config{}, []string{"true"}, "")
			var response CoordinatorHTTPError
			if !errors.As(err, &response) || response.StatusCode != status || calls.Load() != 1 || time.Since(start) >= 500*time.Millisecond {
				t.Fatalf("err=%v calls=%d elapsed=%s", err, calls.Load(), time.Since(start))
			}
		})
	}
}

func TestCoordinatorRunAdmissionAttemptAndOverallBudgets(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "alice@example.test")
	for _, slow := range []bool{false, true} {
		t.Run(fmt.Sprint(slow), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				start := time.Now()
				c := &CoordinatorClient{BaseURL: "https://coordinator.test", readRetryWriter: io.Discard, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if slow {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					return &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": []string{"8"}}, Body: io.NopCloser(strings.NewReader("busy"))}, nil
				})}}
				_, err := c.CreateRun(t.Context(), admissionTestRunID, "", Config{}, []string{"true"}, "")
				wantCalls := 4
				if slow {
					wantCalls = 3
				}
				if err == nil || calls != wantCalls || time.Since(start) > 30*time.Second || (!slow && time.Since(start) != 24*time.Second) || (slow && time.Since(start) != 30*time.Second) {
					t.Fatalf("err=%v calls=%d elapsed=%s", err, calls, time.Since(start))
				}
			})
		})
	}
}
