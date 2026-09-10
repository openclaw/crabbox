package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestCoordinatorPrepareResolveRecovery(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		prepare, release          bool
		id                        string
		status, second, calls     int
		cancel, mismatch, wrongID bool
	}{
		{name: "server failure then success", prepare: true, id: "cbx_123456789abc", status: 500, second: 200, calls: 2},
		{name: "last service diagnostic", prepare: true, id: "cbx_123456789abc", status: 500, second: 503, calls: 2},
		{name: "unauthorized", prepare: true, id: "cbx_123456789abc", status: 401, calls: 1},
		{name: "forbidden", prepare: true, id: "cbx_123456789abc", status: 403, calls: 1},
		{name: "missing", prepare: true, id: "cbx_123456789abc", status: 404, calls: 1},
		{name: "conflict", prepare: true, id: "cbx_123456789abc", status: 409, calls: 1},
		{name: "request timeout status", prepare: true, id: "cbx_123456789abc", status: 408, calls: 1},
		{name: "rate limited", prepare: true, id: "cbx_123456789abc", status: 429, calls: 1},
		{name: "plain observation", id: "cbx_123456789abc", status: 500, calls: 1},
		{name: "release observation", prepare: true, release: true, id: "cbx_123456789abc", status: 500, calls: 1},
		{name: "alias", prepare: true, id: "blue-crab", status: 500, calls: 1},
		{name: "canceled service response", prepare: true, id: "cbx_123456789abc", status: 500, calls: 1, cancel: true},
		{name: "wrong lease", prepare: true, id: "cbx_123456789abc", status: 200, calls: 1, wrongID: true},
		{name: "recovered wrong lease", prepare: true, id: "cbx_123456789abc", status: 500, second: 200, calls: 2, wrongID: true},
		{name: "recovered provider mismatch", prepare: true, id: "cbx_123456789abc", status: 500, second: 200, calls: 2, mismatch: true},
		{name: "ordinary alias resolution", prepare: true, id: "blue-crab", status: 200, calls: 1, wrongID: true},
		{name: "provider mismatch", prepare: true, id: "cbx_123456789abc", status: 200, calls: 1, mismatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("CRABBOX_OWNER", "alice@example.test")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/v1/leases/"+tc.id {
					t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
				}
				calls++
				status := tc.status
				if calls > 1 {
					status = tc.second
				}
				if tc.cancel {
					cancel()
				}
				if status != 200 {
					w.WriteHeader(status)
					io.WriteString(w, "service unavailable")
					return
				}
				provider := "aws"
				if tc.mismatch {
					provider = "gcp"
				}
				id := tc.id
				if tc.wrongID {
					id = "cbx_aaaaaaaaaaaa"
				}
				json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: id, Provider: provider, State: "active"}})
			}))
			defer server.Close()
			b := &coordinatorLeaseBackend{cfg: Config{Provider: "aws"}, coord: &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}}
			lease, err := b.Resolve(ctx, ResolveRequest{ID: tc.id, Prepare: tc.prepare, ReleaseOnly: tc.release})
			if calls != tc.calls {
				t.Fatalf("calls=%d want%d error=%v", calls, tc.calls, err)
			}
			switch {
			case tc.cancel:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error=%v", err)
				}
			case tc.mismatch:
				if !isCoordinatorProviderIdentityError(err) {
					t.Fatalf("error=%v", err)
				}
			case tc.wrongID && tc.prepare && isCanonicalLeaseID(tc.id):
				if ExitCodeForError(err, 0) != 4 || lease.LeaseID != "" {
					t.Fatalf("mismatched lease adopted: lease=%s error=%v", lease.LeaseID, err)
				}
			case tc.status == 200 || tc.second == 200:
				if err != nil {
					t.Fatal(err)
				}
			default:
				var failure CoordinatorHTTPError
				want := tc.status
				if tc.calls > 1 {
					want = tc.second
				}
				if !errors.As(err, &failure) || failure.StatusCode != want || failure.Message != "service unavailable" {
					t.Fatalf("error=%v want typed status%d", err, want)
				}
			}
		})
	}
}

func TestCoordinatorPrepareResolveSharesOriginalDeadline(t *testing.T) {
	for _, callerTimeout := range []time.Duration{0, 5 * time.Second} {
		t.Run(callerTimeout.String(), func(t *testing.T) {
			t.Setenv("CRABBOX_OWNER", "alice@example.test")
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				budget := 10 * time.Second
				ctx := t.Context()
				if callerTimeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, callerTimeout)
					defer cancel()
					budget = callerTimeout
				}
				calls := 0
				client := &http.Client{Timeout: 10 * time.Second, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					if deadline, ok := req.Context().Deadline(); !ok || !deadline.Equal(start.Add(budget)) {
						t.Errorf("deadline=%v present=%v", deadline, ok)
					}
					if calls == 1 {
						time.Sleep(2 * time.Second)
						return &http.Response{StatusCode: 500, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("temporary"))}, nil
					}
					<-req.Context().Done()
					return nil, req.Context().Err()
				})}
				b := &coordinatorLeaseBackend{cfg: Config{Provider: "aws"}, coord: &CoordinatorClient{BaseURL: "https://broker.example.test", Client: client}}
				_, err := b.Resolve(ctx, ResolveRequest{ID: "cbx_123456789abc", Prepare: true})
				if calls != 2 || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != budget {
					t.Fatalf("calls=%d elapsed=%v error=%v", calls, time.Since(start), err)
				}
			})
		})
	}
}

func TestCoordinatorPrepareResolveDoesNotRetryInitialTimeout(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "alice@example.test")
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			<-req.Context().Done()
			return nil, req.Context().Err()
		})}
		backend := &coordinatorLeaseBackend{cfg: Config{Provider: "aws"}, coord: &CoordinatorClient{BaseURL: "https://broker.example.test", Client: client}}
		_, err := backend.Resolve(t.Context(), ResolveRequest{ID: "cbx_123456789abc", Prepare: true})
		if calls != 1 || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("calls=%d error=%v", calls, err)
		}
	})
}

func TestCoordinatorPrepareResolveRejectsCanceledRecoveryResponse(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "alice@example.test")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		status, body := 500, "temporary"
		if calls == 2 {
			cancel()
			status, body = 200, `{"lease":{"id":"cbx_123456789abc","provider":"aws","state":"active"}}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	backend := &coordinatorLeaseBackend{cfg: Config{Provider: "aws"}, coord: &CoordinatorClient{BaseURL: "https://broker.example.test", Client: client}}
	lease, err := backend.Resolve(ctx, ResolveRequest{ID: "cbx_123456789abc", Prepare: true})
	if calls != 2 || !errors.Is(err, context.Canceled) || lease.LeaseID != "" {
		t.Fatalf("calls=%d lease=%s error=%v", calls, lease.LeaseID, err)
	}
}

func TestCoordinatorPrepareResolveSharesControlBudget(t *testing.T) {
	for _, noHTTPTimeout := range []bool{false, true} {
		name := "production HTTP timeout"
		if noHTTPTimeout {
			name = "no HTTP timeout"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("CRABBOX_OWNER", "alice@example.test")
			synctest.Test(t, func(t *testing.T) {
				coord := mustNewCoordinatorClient(t, Config{Coordinator: "https://broker.example.test"})
				if coord.Client.Timeout != 30*time.Minute {
					t.Fatalf("production HTTP timeout=%v", coord.Client.Timeout)
				}
				if noHTTPTimeout {
					coord.Client.Timeout = 0
				}
				start := time.Now()
				calls := 0
				coord.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					deadline, ok := req.Context().Deadline()
					t.Logf("request=%d method=%s path=%s elapsed=%s deadlineRemaining=%s", calls, req.Method, req.URL.Path, time.Since(start), time.Until(deadline))
					if !ok || !deadline.Equal(start.Add(30*time.Second)) {
						t.Errorf("request%d deadline=%v, want original control deadline", calls, deadline.Sub(start))
					}
					if calls == 1 {
						time.Sleep(25 * time.Second)
						t.Logf("response=500 elapsed=%s", time.Since(start))
						return &http.Response{StatusCode: 500, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("temporary coordinator failure"))}, nil
					}
					<-req.Context().Done()
					t.Logf("response=deadline elapsed=%s", time.Since(start))
					return nil, req.Context().Err()
				})
				backend := &coordinatorLeaseBackend{cfg: Config{Provider: "aws"}, coord: coord}
				lease, err := backend.Resolve(t.Context(), ResolveRequest{ID: "cbx_123456789abc", Prepare: true})
				if calls != 2 || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 30*time.Second || lease.LeaseID != "" {
					t.Fatalf("calls=%d elapsed=%s lease=%s error=%v", calls, time.Since(start), lease.LeaseID, err)
				}
			})
		})
	}
}
