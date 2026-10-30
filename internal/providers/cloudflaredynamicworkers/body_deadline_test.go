package cloudflaredynamicworkers

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/openclaw/crabbox/internal/testutil"
)

func TestResponseBodyBudgetStartsAfterHeaders(t *testing.T) {
	for _, operation := range []string{"readiness", "run"} {
		t.Run(operation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const budget = 25 * time.Millisecond
				server := testutil.NewPipeHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					time.Sleep(2 * budget)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					if operation == "run" {
						_, _ = w.Write([]byte(`{"id":"run_budget","status":`))
					} else {
						_, _ = w.Write([]byte(`{"ok":`))
					}
					w.(http.Flusher).Flush()
					time.Sleep(budget / 2)
					if operation == "run" {
						_, _ = w.Write([]byte(`"succeeded","exitCode":0}`))
					} else {
						_, _ = w.Write([]byte(`true}`))
					}
				}))
				defer server.Close()
				c := &client{baseURL: server.URL, http: server.Client(), responseBodyTimeout: budget}
				start := time.Now()
				if operation == "run" {
					out, err := c.Run(t.Context(), runRequest{ID: "run_budget"})
					if err != nil || out.ID != "run_budget" || out.Status != "succeeded" {
						t.Fatalf("run=%+v err=%v", out, err)
					}
				} else {
					out, err := c.Readiness(t.Context())
					if err != nil || !out.OK {
						t.Fatalf("readiness=%+v err=%v", out, err)
					}
				}
				if elapsed := time.Since(start); elapsed != 2*budget+budget/2 {
					t.Fatalf("elapsed=%s; header wait must not consume body budget", elapsed)
				}
			})
		})
	}
}

func TestResponseBodyCallerCancellationReleasesStalledRequest(t *testing.T) {
	for _, operation := range []string{"readiness", "run"} {
		t.Run(operation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				type valueKey struct{}
				caller, cancelDeadline := context.WithTimeout(context.WithValue(t.Context(), valueKey{}, "request-value"), time.Second)
				defer cancelDeadline()
				ctx, cancel := context.WithCancel(caller)
				defer cancel()
				started, ended := make(chan struct{}), make(chan struct{})
				server := testutil.NewPipeHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"unfinished":`))
					w.(http.Flusher).Flush()
					close(started)
					<-r.Context().Done()
					close(ended)
				}))
				defer server.Close()
				transport := server.Client().Transport
				observed := false
				observedClient := &http.Client{Transport: testutil.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
					observed = true
					gotDeadline, gotOK := req.Context().Deadline()
					wantDeadline, wantOK := ctx.Deadline()
					if req.Context().Value(valueKey{}) != "request-value" || gotOK != wantOK || !gotDeadline.Equal(wantDeadline) {
						t.Error("derived request lost caller value or deadline")
					}
					return transport.RoundTrip(req)
				})}
				c := &client{baseURL: server.URL, http: observedClient, responseBodyTimeout: 500 * time.Millisecond}
				go func() { <-started; time.Sleep(10 * time.Millisecond); cancel() }()
				var err error
				if operation == "run" {
					_, err = c.Run(ctx, runRequest{ID: "run_cancel"})
				} else {
					_, err = c.Readiness(ctx)
				}
				if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("caller cancellation changed: %v", err)
				}
				synctest.Wait()
				select {
				case <-ended:
				default:
					t.Fatal("request handler survived caller cancellation")
				}
				if !observed {
					t.Fatal("request was not dispatched through injected transport")
				}
			})
		})
	}
}

func TestResponseBodyDeadlinePreservesCompletedConnectionReuse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := testutil.NewPipeHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(readinessResponse{OK: true})
		}))
		defer server.Close()
		transport := server.Client().Transport.(*http.Transport)
		dial := transport.DialContext
		var dials atomic.Int32
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			dials.Add(1)
			return dial(ctx, network, address)
		}
		c := &client{baseURL: server.URL, http: server.Client(), responseBodyTimeout: 25 * time.Millisecond}
		for range 2 {
			out, err := c.Readiness(t.Context())
			if err != nil || !out.OK {
				t.Fatalf("readiness=%+v err=%v", out, err)
			}
		}
		if transport.DisableKeepAlives || dials.Load() != 1 {
			t.Fatalf("completed responses did not reuse their connection: keepAliveDisabled=%t dials=%d", transport.DisableKeepAlives, dials.Load())
		}
	})
}

func TestResponseBodyRetryCancelsOnlyItsOwnAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		firstEnded := make(chan struct{})
		calls := 0
		server := testutil.NewPipeHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("incomplete error body"))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(firstEnded)
				return
			}
			<-firstEnded
			_ = json.NewEncoder(w).Encode(runStatus{ID: "run_retry", Status: "succeeded"})
		}))
		defer server.Close()
		transport := server.Client().Transport
		var firstContext context.Context
		dispatches := 0
		observedClient := &http.Client{Transport: testutil.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
			dispatches++
			if dispatches == 1 {
				firstContext = req.Context()
			} else {
				if req.Context() == firstContext || !errors.Is(firstContext.Err(), context.Canceled) || req.Context().Err() != nil {
					t.Error("retry reused or inherited canceled attempt context")
				}
			}
			return transport.RoundTrip(req)
		})}
		c := &client{baseURL: server.URL, http: observedClient, responseBodyTimeout: time.Second, readRetryDelays: []time.Duration{0}}
		parent := t.Context()
		out, err := c.Status(parent, "run_retry")
		if err != nil || out.Status != "succeeded" || dispatches != 2 || parent.Err() != nil {
			t.Fatalf("status=%+v calls=%d parent=%v err=%v", out, dispatches, parent.Err(), err)
		}
	})
}

func TestResponseBodyDeadlineDoesNotCancelFollowingRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		firstEnded := make(chan struct{})
		calls := 0
		server := testutil.NewPipeHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				_, _ = w.Write([]byte(`{"ok":`))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(firstEnded)
				return
			}
			<-firstEnded
			_ = json.NewEncoder(w).Encode(readinessResponse{OK: true})
		}))
		defer server.Close()
		c := &client{baseURL: server.URL, http: server.Client(), responseBodyTimeout: 25 * time.Millisecond}
		if _, err := c.Readiness(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("first request error=%v", err)
		}
		out, err := c.Readiness(t.Context())
		if err != nil || !out.OK || calls != 2 || t.Context().Err() != nil {
			t.Fatalf("following request=%+v calls=%d parent=%v err=%v", out, calls, t.Context().Err(), err)
		}
	})
}
