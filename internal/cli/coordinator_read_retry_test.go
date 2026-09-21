package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func TestCoordinatorReadRetryPublicReads(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "alice@example.test")
	for _, tc := range []struct {
		name string
		call func(context.Context, *CoordinatorClient) error
	}{
		{"lease", func(ctx context.Context, c *CoordinatorClient) error {
			_, err := c.GetLease(ctx, "cbx_read")
			return err
		}},
		{"metadata", func(ctx context.Context, c *CoordinatorClient) error {
			_, err := c.GetLeaseWithAuthoritativeProviderMetadata(ctx, "cbx_read")
			return err
		}},
		{"list", func(ctx context.Context, c *CoordinatorClient) error {
			_, err := c.Leases(ctx, "active", 10)
			return err
		}},
		{"history", func(ctx context.Context, c *CoordinatorClient) error {
			_, err := c.Runs(ctx, "", "", "", "", 10)
			return err
		}},
		{"run", func(ctx context.Context, c *CoordinatorClient) error { _, err := c.Run(ctx, "run_read"); return err }},
		{"events", func(ctx context.Context, c *CoordinatorClient) error {
			_, err := c.RunEvents(ctx, "run_read", 0, 10)
			return err
		}},
		{"pool", func(ctx context.Context, c *CoordinatorClient) error {
			_, err := c.Pool(ctx, Config{Provider: "aws"})
			return err
		}},
		{"admin list", func(ctx context.Context, c *CoordinatorClient) error {
			_, err := c.AdminLeases(ctx, "active", "", "", 10)
			return err
		}},
		{"lease audit", func(ctx context.Context, c *CoordinatorClient) error {
			_, err := c.AdminLeaseAudit(ctx, "active", "aws", "", "", 10)
			return err
		}},
		{"whoami", func(ctx context.Context, c *CoordinatorClient) error { _, err := c.Whoami(ctx); return err }},
		{"health", func(ctx context.Context, c *CoordinatorClient) error { return c.Health(ctx) }},
		{"readiness", func(ctx context.Context, c *CoordinatorClient) error {
			_, err := c.ProviderReadiness(ctx, Config{Provider: "aws"})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("method=%s", r.Method)
				}
				if calls.Add(1) == 1 {
					http.Error(w, "busy", http.StatusBadGateway)
					return
				}
				io.WriteString(w, `{}`)
			}))
			defer server.Close()
			var progress bytes.Buffer
			c := &CoordinatorClient{BaseURL: server.URL, Client: server.Client(), readRetryWriter: &progress}
			if err := tc.call(t.Context(), c); err != nil || calls.Load() != 2 {
				t.Fatalf("err=%v requests=%d", err, calls.Load())
			}
			if progress.String() != "coordinator read retry 1/4 reason=http_502\n" {
				t.Fatalf("progress=%q", progress.String())
			}
		})
	}
}

func TestCoordinatorReadRetryStatusAndAfter(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "alice@example.test")
	for _, tc := range []struct {
		name             string
		status           int
		after            string
		minimum, maximum time.Duration
	}{
		{"503", 503, "", time.Second, 3 * time.Second},
		{"504", 504, "", time.Second, 3 * time.Second},
		{"429 after", 429, "2", 2 * time.Second, 4 * time.Second},
		{"429 capped", 429, "3600", 8 * time.Second, 10 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			times := make(chan time.Time, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				times <- time.Now()
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", tc.after)
					http.Error(w, "busy", tc.status)
					return
				}
				io.WriteString(w, `{"lease":{"id":"cbx_read"}}`)
			}))
			defer server.Close()
			c := &CoordinatorClient{BaseURL: server.URL, Client: server.Client(), readRetryWriter: io.Discard}
			lease, err := c.GetLease(t.Context(), "cbx_read")
			if calls.Load() != 2 {
				t.Fatalf("err=%v requests=%d", err, calls.Load())
			}
			first, second := <-times, <-times
			delay := second.Sub(first)
			if err != nil || lease.ID != "cbx_read" || calls.Load() != 2 || delay < tc.minimum || delay > tc.maximum {
				t.Fatalf("lease=%s err=%v requests=%d delay=%s", lease.ID, err, calls.Load(), delay)
			}
		})
	}
}

func TestCoordinatorReadRetryRejectsTerminalResponsesAndMutations(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "alice@example.test")
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, status := range []int{400, 401, 403, 404, 409, 500, 429, 502, 503, 504} {
			if method == http.MethodGet && (status == 429 || status >= 502) {
				continue
			}
			t.Run(fmt.Sprintf("%s/%d", method, status), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					http.Error(w, "denied", status)
				}))
				defer server.Close()
				c := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
				var err error
				if method == http.MethodGet {
					err = c.doRead(t.Context(), "/v1/health", nil)
				} else {
					err = c.do(t.Context(), method, "/v1/leases", nil, nil)
				}
				var response CoordinatorHTTPError
				if !errors.As(err, &response) || response.StatusCode != status || calls.Load() != 1 {
					t.Fatalf("err=%v requests=%d", err, calls.Load())
				}
			})
		}
	}
}

func TestCoordinatorReadRetryBodyTimeoutThenSuccess(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "alice@example.test")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			io.WriteString(w, "partial log")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		io.WriteString(w, "complete log")
	}))
	defer server.Close()
	c := &CoordinatorClient{BaseURL: server.URL, Client: &http.Client{Timeout: 50 * time.Millisecond}, readRetryWriter: io.Discard}
	logs, err := c.RunLogs(t.Context(), "run_read")
	if err != nil || logs != "complete log" || calls.Load() != 2 {
		t.Fatalf("logs=%q err=%v requests=%d", logs, err, calls.Load())
	}
}

func TestCoordinatorReadRetryCancellation(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "alice@example.test")
	for _, during := range []string{"request", "backoff"} {
		t.Run(during, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered := make(chan struct{})
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if during == "request" {
					close(entered)
					<-r.Context().Done()
					return
				}
				w.Header().Set("Retry-After", "3600")
				http.Error(w, "busy", 429)
			}))
			defer server.Close()
			writer := io.Writer(io.Discard)
			if during == "backoff" {
				writer = coordinatorRetryNotifyWriter{entered}
			}
			c := &CoordinatorClient{BaseURL: server.URL, Client: server.Client(), readRetryWriter: writer}
			done := make(chan error, 1)
			go func() { done <- c.Health(ctx) }()
			<-entered
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
					t.Fatalf("err=%v requests=%d", err, calls.Load())
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not stop read promptly")
			}
		})
	}
}

type coordinatorRetryNotifyWriter struct{ entered chan struct{} }

func (w coordinatorRetryNotifyWriter) Write(p []byte) (int, error) {
	close(w.entered)
	return len(p), nil
}

func TestCoordinatorReadRetryBudgets(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "alice@example.test")
	for _, budget := range []time.Duration{5 * time.Second, time.Minute} {
		t.Run(budget.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				if budget < time.Minute {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, budget)
					defer cancel()
				}
				start := time.Now()
				calls := 0
				c := &CoordinatorClient{BaseURL: "https://broker.example.test", readRetryWriter: io.Discard, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					<-r.Context().Done()
					return nil, r.Context().Err()
				})}}
				err := c.Health(ctx)
				wantCalls := 2
				if budget < time.Minute {
					wantCalls = 1
				}
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != budget || calls != wantCalls {
					t.Fatalf("err=%v elapsed=%s calls=%d", err, time.Since(start), calls)
				}
			})
		})
	}
}

func TestCoordinatorReadRetryExhaustionAndProgress(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "alice@example.test")
	synctest.Test(t, func(t *testing.T) {
		var progress bytes.Buffer
		calls := 0
		start := time.Now()
		c := &CoordinatorClient{BaseURL: "https://broker.example.test", readRetryWriter: &progress, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("busy"))}, nil
		})}}
		err := c.Health(t.Context())
		var response CoordinatorHTTPError
		if !errors.As(err, &response) || response.StatusCode != 503 || calls != 5 || time.Since(start) < 15*time.Second || time.Since(start) > 18*time.Second || strings.Count(progress.String(), "coordinator read retry") != 1 {
			t.Fatalf("err=%v elapsed=%s calls=%d progress=%q", err, time.Since(start), calls, progress.String())
		}
	})
}

func TestCoordinatorReadRetryAfterParsingAndConnectionReset(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"2", 2 * time.Second}, {"9223372036854775807", 8 * time.Second}, {"-1", 0}, {"invalid", 0}, {"", 0},
		{now.Add(3 * time.Second).Format(http.TimeFormat), 3 * time.Second},
		{now.Add(time.Hour).Format(http.TimeFormat), 8 * time.Second},
		{now.Add(-time.Hour).Format(http.TimeFormat), 0},
	} {
		if got := coordinatorReadRetryAfter(tc.value, now); got != tc.want {
			t.Errorf("Retry-After %q=%s want=%s", tc.value, got, tc.want)
		}
	}
	if got := coordinatorReadRetryReason(&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}); got != "connection_reset" {
		t.Fatalf("reset reason=%q", got)
	}
}

func TestCoordinatorReadRetryCurlResponsePolicy(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "alice@example.test")
	t.Setenv("NO_PROXY", "127.0.0.1")
	t.Setenv("no_proxy", "127.0.0.1")
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is unavailable")
	}
	for _, status := range []int{404, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			times := make(chan time.Time, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				times <- time.Now()
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", "2")
					http.Error(w, "fallback response", status)
					return
				}
				io.WriteString(w, `{"lease":{"id":"cbx_read"}}`)
			}))
			defer server.Close()
			c := &CoordinatorClient{BaseURL: server.URL, readRetryWriter: io.Discard, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return nil, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
			})}}
			lease, err := c.GetLease(t.Context(), "cbx_read")
			if status == 404 {
				var response CoordinatorHTTPError
				if !errors.As(err, &response) || response.StatusCode != 404 || calls.Load() != 1 {
					t.Fatalf("err=%v calls=%d", err, calls.Load())
				}
			} else {
				if err != nil || lease.ID != "cbx_read" || calls.Load() != 2 {
					t.Fatalf("lease=%s err=%v calls=%d", lease.ID, err, calls.Load())
				}
				first, second := <-times, <-times
				if delay := second.Sub(first); delay < 2*time.Second || delay > 4*time.Second {
					t.Fatalf("fallback Retry-After delay=%s", delay)
				}
			}
		})
	}
}
