package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type listObservedWriter struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	needle  string
	written chan struct{}
	once    sync.Once
}

func (w *listObservedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buffer.Write(p)
	if strings.Contains(w.buffer.String(), w.needle) {
		w.once.Do(func() { close(w.written) })
	}
	return n, err
}

func (w *listObservedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

func listTestConfig(t *testing.T, url string) {
	t.Helper()
	clearConfigEnv(t)
	t.Setenv("CRABBOX_COORDINATOR", url)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "synthetic-user")
	t.Setenv("CRABBOX_OWNER", "alice@example.test")
}

func TestListStreamingRetainedBeforeLaterPagesLiveFirstAndDeduplicates(t *testing.T) {
	for _, format := range []string{"text", "jsonl"} {
		t.Run(format, func(t *testing.T) {
			deleted := true
			out := &listObservedWriter{needle: "cbx_first", written: make(chan struct{})}
			var stderr bytes.Buffer
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if (q.Get("view") == "current") != (q.Get("state") == "") || q.Get("pagination") != "keyset-v1" || q.Get("provider") != "aws" || q.Get("projection") != "summary" || q.Get("limit") != "100" {
					t.Errorf("unexpected query %s", r.URL.RawQuery)
				}
				if format == "text" && !strings.HasPrefix(out.String(), "ID ") {
					t.Error("header was not printed before the first request")
				}
				res := map[string]any{"pagination": "keyset-v1"}
				switch q.Get("state") {
				case "active":
					if q.Get("cursor") == "" {
						res["leases"] = []CoordinatorLease{{ID: "cbx_first", Provider: "aws", State: "active", CloudID: "cbx_first", Pond: "alpha"}}
						res["nextCursor"] = "page-two"
					} else {
						// Page two cannot finish until page one's row has reached
						// the command writer, rather than merely an internal buffer.
						select {
						case <-out.written:
						case <-time.After(2 * time.Second):
							t.Error("page one was buffered behind page two")
						}
						res["leases"] = []CoordinatorLease{{ID: "cbx_first", Provider: "aws", State: "active"}, {ID: "cbx_second", Provider: "aws", State: "active", CloudID: "cbx_second", Pond: "alpha"}}
					}
				case "provisioning":
					res["leases"] = []CoordinatorLease{{ID: "cbx_first", Provider: "aws", State: "provisioning", Pond: "alpha"}, {ID: "cbx_provisioning", Provider: "aws", State: "provisioning", CloudID: "cbx_provisioning", Pond: "alpha"}}
				case "":
					res["leases"] = []CoordinatorLease{
						{ID: "cbx_first", Provider: "aws", State: "released", Keep: true, Pond: "alpha"},
						{ID: "cbx_retained", Provider: "aws", State: "released", Keep: true, CloudID: "cbx_retained", Pond: "alpha"},
						{ID: "cbx_wrong_pond", Provider: "aws", State: "released", Keep: true, Pond: "beta"},
						{ID: "cbx_deleted", Provider: "aws", State: "released", Keep: true, CleanupStatus: "complete", CleanupCompletedAt: "2026-01-01T00:00:00Z", ReleaseDeletesServer: &deleted, Pond: "alpha"},
						{ID: "cbx_wrong_provider", Provider: "azure", State: "active", Pond: "alpha"},
					}
				default:
					t.Errorf("unexpected state %s", q.Get("state"))
				}
				_ = json.NewEncoder(w).Encode(res)
			}))
			defer server.Close()
			listTestConfig(t, server.URL)
			args := []string{"--provider", "aws", "--pond", "alpha", "--retained"}
			if format == "jsonl" {
				args = append(args, "--jsonl")
			}
			if err := (App{Stdout: out, Stderr: &stderr}).list(t.Context(), args); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			ids := []string{"cbx_first", "cbx_second", "cbx_provisioning", "cbx_retained"}
			if format == "jsonl" {
				lines := strings.Split(strings.TrimSpace(got), "\n")
				if len(lines) != len(ids) {
					t.Fatalf("lines=%d, want %d", len(lines), len(ids))
				}
				for i, line := range lines {
					var lease CoordinatorLease
					if err := json.Unmarshal([]byte(line), &lease); err != nil || lease.ID != ids[i] {
						t.Fatalf("line %d id=%s err=%v", i, lease.ID, err)
					}
				}
			} else {
				last := -1
				for _, id := range ids {
					index := strings.Index(got, "lease="+id+" ")
					if index <= last || strings.Count(got, "lease="+id+" ") != 1 {
						t.Fatalf("wrong order or duplicate %s: %s", id, got)
					}
					last = index
				}
				separator := strings.Index(got, "retained (cleanup not confirmed):")
				if separator < strings.Index(got, "lease=cbx_provisioning ") || separator > strings.Index(got, "lease=cbx_retained ") {
					t.Fatalf("misplaced separator: %s", got)
				}
			}
			if stderr.Len() != 0 || strings.Contains(got, "cbx_wrong") || strings.Contains(got, "cbx_deleted") {
				t.Fatalf("unexpected output stdout=%s stderr=%s", got, &stderr)
			}
		})
	}
}

func TestListStreamingDefaultStopsAfterLiveQueries(t *testing.T) {
	for _, format := range []string{"--json", "--jsonl", ""} {
		t.Run(format, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				state := r.URL.Query().Get("state")
				if state != "active" && state != "provisioning" || r.URL.Query().Get("view") == "current" {
					t.Error("default requested retained history")
				}
				createdAt := "2026-01-01"
				if state == "provisioning" {
					createdAt = "2026-02-01"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"pagination": "keyset-v1", "leases": []CoordinatorLease{{ID: "cbx_" + state, Provider: "aws", State: state, CreatedAt: createdAt}}})
			}))
			defer server.Close()
			listTestConfig(t, server.URL)
			var stdout, stderr bytes.Buffer
			args := []string{"--provider", "aws"}
			if format != "" {
				args = append(args, format)
			}
			if err := (App{Stdout: &stdout, Stderr: &stderr}).list(t.Context(), args); err != nil || calls.Load() != 2 {
				t.Fatalf("err=%v calls=%d stderr=%s", err, calls.Load(), &stderr)
			}
			if format == "--json" {
				var leases []CoordinatorLease
				if err := json.Unmarshal(stdout.Bytes(), &leases); err != nil || len(leases) != 2 || leases[0].ID != "cbx_provisioning" || leases[1].ID != "cbx_active" {
					t.Fatalf("array=%s err=%v", &stdout, err)
				}
			}
		})
	}
}

func TestListStreamingRetainedJSONKeepsBufferedNewestFirstArray(t *testing.T) {
	var stdout, stderr bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if stdout.Len() != 0 {
			t.Error("--json emitted a partial array")
		}
		res := map[string]any{"pagination": "keyset-v1"}
		if q.Get("state") == "active" {
			res["leases"] = []CoordinatorLease{{ID: "old", Provider: "aws", State: "active", CreatedAt: "2026-01-01"}}
		} else if q.Get("state") == "provisioning" {
			res["leases"] = []CoordinatorLease{}
		} else if q.Get("cursor") == "" {
			res["leases"] = []CoordinatorLease{{ID: "old", Provider: "aws", State: "active", CreatedAt: "2026-01-01"}}
			res["nextCursor"] = "next"
		} else {
			res["leases"] = []CoordinatorLease{{ID: "new", Provider: "aws", State: "released", Keep: true, CreatedAt: "2026-02-01"}}
		}
		_ = json.NewEncoder(w).Encode(res)
	}))
	defer server.Close()
	listTestConfig(t, server.URL)
	if err := (App{Stdout: &stdout, Stderr: &stderr}).list(t.Context(), []string{"--provider", "aws", "--json", "--retained"}); err != nil {
		t.Fatal(err)
	}
	var leases []CoordinatorLease
	if err := json.Unmarshal(stdout.Bytes(), &leases); err != nil || len(leases) != 2 || leases[0].ID != "new" || leases[1].ID != "old" {
		t.Fatalf("array=%s err=%v", &stdout, err)
	}
}

func TestListStreamingSectionsBoundedConcurrencyAndStableOrder(t *testing.T) {
	var inFlight, maximum, started atomic.Int32
	fourStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		if started.Add(1) == 4 {
			once.Do(func() { close(fourStarted) })
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"pagination": "keyset-v1", "leases": []CoordinatorLease{{ID: r.URL.Query().Get("provider")}}})
	}))
	defer server.Close()
	queries := make([]coordinatorLeaseQuery, 9)
	for i := range queries {
		queries[i] = coordinatorLeaseQuery{provider: strconv.Itoa(i), current: true}
	}
	c := &CoordinatorClient{BaseURL: server.URL, Client: server.Client(), readRetryWriter: io.Discard}
	var order []string
	done := make(chan error, 1)
	go func() {
		_, err := c.streamLeaseSections(t.Context(), queries, func(_ int, leases []CoordinatorLease, _ bool) error {
			order = append(order, leases[0].ID)
			return nil
		})
		done <- err
	}()
	select {
	case <-fourStarted:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("sections were not fetched concurrently")
	}
	close(release)
	if err := <-done; err != nil || maximum.Load() != 4 || fmt.Sprint(order) != "[0 1 2 3 4 5 6 7 8]" {
		t.Fatalf("err=%v max=%d order=%v", err, maximum.Load(), order)
	}
}

func TestListMachinesStreamsPast1000WithoutCap(t *testing.T) {
	for _, format := range []string{"--jsonl", "--json"} {
		t.Run(format, func(t *testing.T) {
			out := &listObservedWriter{needle: `"id":"machine-0"`, written: make(chan struct{})}
			var pages atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/pool" {
					machines := make([]CoordinatorMachine, 1103)
					for i := range machines {
						machines[i] = CoordinatorMachine{ID: CoordinatorID(fmt.Sprintf("machine-%d", i)), Provider: "aws", Labels: map[string]string{"lease": fmt.Sprintf("lease-%d", i)}}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"machines": machines})
					return
				}
				q := r.URL.Query()
				if r.URL.Path != "/v1/admin/leases" || q.Get("pagination") != "keyset-v1" || q.Get("limit") != "100" {
					t.Errorf("unpaginated admin lookup: %s", r.URL)
				}
				if q.Get("state") == "provisioning" {
					_ = json.NewEncoder(w).Encode(map[string]any{"pagination": "keyset-v1", "leases": []CoordinatorLease{{ID: "lease-1101", State: "provisioning"}}})
					return
				}
				pages.Add(1)
				start, _ := strconv.Atoi(q.Get("cursor"))
				if start == 100 && format == "--jsonl" {
					select {
					case <-out.written:
					case <-time.After(2 * time.Second):
						t.Error("--all buffered the first machine behind later lease pages")
					}
				}
				res := map[string]any{"pagination": "keyset-v1"}
				leases := make([]CoordinatorLease, 0, 100)
				for i := start; i < min(start+100, 1101); i++ {
					leases = append(leases, CoordinatorLease{ID: fmt.Sprintf("lease-%d", i), State: "active"})
				}
				res["leases"] = leases
				if start+100 < 1101 {
					res["nextCursor"] = strconv.Itoa(start + 100)
				}
				_ = json.NewEncoder(w).Encode(res)
			}))
			defer server.Close()
			listTestConfig(t, server.URL)
			t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "synthetic-admin")
			var stderr bytes.Buffer
			if err := (App{Stdout: out, Stderr: &stderr}).list(t.Context(), []string{"--provider", "aws", "--all", "--retained", format}); err != nil {
				t.Fatal(err)
			}
			var machines []CoordinatorMachine
			if format == "--json" {
				if err := json.Unmarshal([]byte(out.String()), &machines); err != nil {
					t.Fatal(err)
				}
			} else {
				for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
					var machine CoordinatorMachine
					if err := json.Unmarshal([]byte(line), &machine); err != nil {
						t.Fatal(err)
					}
					machines = append(machines, machine)
				}
			}
			if len(machines) != 1103 || pages.Load() != 12 || stderr.Len() != 0 {
				t.Fatalf("machines=%d pages=%d stderr=%s", len(machines), pages.Load(), &stderr)
			}
		})
	}
}

func TestListStreamingReadRetriesRemainUnchanged(t *testing.T) {
	t.Setenv("CRABBOX_OWNER", "alice@example.test")
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		start := time.Now()
		var retries bytes.Buffer
		c := &CoordinatorClient{BaseURL: "https://example.test", readRetryWriter: &retries, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			code, body := 200, `{"pagination":"keyset-v1","leases":[{"id":"after-retry"}]}`
			header := make(http.Header)
			if calls == 1 {
				code, body = 503, `{"error":"coordinator_unavailable"}`
				header.Set("Retry-After", "2")
			} else if calls == 2 {
				code, body = 500, "error code: 1101"
			}
			return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}}
		var ids []string
		_, err := c.streamLeaseSections(t.Context(), []coordinatorLeaseQuery{{current: true}}, func(_ int, leases []CoordinatorLease, _ bool) error {
			ids = append(ids, leases[0].ID)
			return nil
		})
		if err != nil || calls != 3 || fmt.Sprint(ids) != "[after-retry]" || time.Since(start) < 4*time.Second || time.Since(start) > 6*time.Second {
			t.Fatalf("err=%v calls=%d ids=%v elapsed=%s", err, calls, ids, time.Since(start))
		}
	})
}

func TestListStreamingSectionsCancelOnRenderFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"pagination": "keyset-v1", "leases": []CoordinatorLease{{ID: "first"}}, "nextCursor": "later"})
	}))
	defer server.Close()
	c := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	want := errors.New("writer closed")
	_, err := c.streamLeaseSections(t.Context(), []coordinatorLeaseQuery{{current: true}}, func(_ int, _ []CoordinatorLease, _ bool) error { return want })
	if !errors.Is(err, want) || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestListStreamingAdminFallbackAndUnavailableLookup(t *testing.T) {
	for _, mode := range []string{"unauthorized", "missing admin", "lookup failure"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/pool":
					if mode == "unauthorized" {
						http.Error(w, "unauthorized", 401)
					} else {
						_ = json.NewEncoder(w).Encode(map[string]any{"machines": []CoordinatorMachine{{ID: "inventory", Labels: map[string]string{"lease": "unverified"}}}})
					}
				case "/v1/admin/leases":
					http.Error(w, "forbidden", 403)
				case "/v1/leases":
					_ = json.NewEncoder(w).Encode(map[string]any{"pagination": "keyset-v1", "leases": []CoordinatorLease{{ID: "visible", Provider: "aws", State: r.URL.Query().Get("state")}}})
				default:
					t.Errorf("unexpected path %s", r.URL)
				}
			}))
			defer server.Close()
			listTestConfig(t, server.URL)
			if mode != "missing admin" {
				t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "synthetic-admin")
			}
			var stdout, stderr bytes.Buffer
			if err := (App{Stdout: &stdout, Stderr: &stderr}).list(t.Context(), []string{"--provider", "aws", "--all"}); err != nil {
				t.Fatal(err)
			}
			if mode == "lookup failure" {
				if !strings.Contains(stdout.String(), "inventory") || strings.Contains(stdout.String(), "orphan=") || !strings.Contains(stderr.String(), "orphan status unavailable") {
					t.Fatalf("stdout=%s stderr=%s", &stdout, &stderr)
				}
			} else if !strings.Contains(stdout.String(), "lease=visible") || !strings.Contains(stderr.String(), "falling back to user-visible leases") {
				t.Fatalf("stdout=%s stderr=%s", &stdout, &stderr)
			}
		})
	}
}

func TestListStreamingAllIncludesUnmatchedInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/pool" {
			_ = json.NewEncoder(w).Encode(map[string]any{"machines": []CoordinatorMachine{
				{ID: "live-machine", Labels: map[string]string{"lease": "live"}},
				{ID: "other-machine", Labels: map[string]string{"lease": "old"}},
			}})
			return
		}
		state := r.URL.Query().Get("state")
		if r.URL.Path != "/v1/admin/leases" || state != "active" && state != "provisioning" {
			t.Errorf("unexpected query %s", r.URL)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"pagination": "keyset-v1", "leases": []CoordinatorLease{{ID: "live", State: state}}})
	}))
	defer server.Close()
	listTestConfig(t, server.URL)
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "synthetic-admin")
	for _, retained := range []bool{false, true} {
		for _, format := range []string{"", "--json", "--jsonl"} {
			t.Run(fmt.Sprintf("retained=%t/%s", retained, format), func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				args := []string{"--provider", "aws", "--all"}
				if retained {
					args = append(args, "--retained")
				}
				if format != "" {
					args = append(args, format)
				}
				if err := (App{Stdout: &stdout, Stderr: &stderr}).list(t.Context(), args); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(stdout.String(), "live-machine") || !strings.Contains(stdout.String(), "other-machine") {
					t.Fatalf("stdout=%s stderr=%s", &stdout, &stderr)
				}
				if format == "" && !strings.Contains(stdout.String(), "other provider inventory:") {
					t.Fatalf("unexpected inventory section: %s", &stdout)
				}
			})
		}
	}
}

func TestListStreamingLegacyCoordinatorFiltersLocally(t *testing.T) {
	for _, retained := range []bool{false, true} {
		for _, format := range []string{"", "--json", "--jsonl"} {
			t.Run(fmt.Sprintf("retained=%t/%s", retained, format), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !retained && (r.URL.Query().Get("state") == "" || r.URL.Query().Get("view") == "current") {
						t.Errorf("default requested history: %s", r.URL)
					}
					// Older coordinators may ignore both state and view filters.
					_ = json.NewEncoder(w).Encode(map[string]any{"leases": []CoordinatorLease{
						{ID: "cbx_active", Provider: "aws", State: "active"},
						{ID: "cbx_pending", Provider: "aws", State: "provisioning"},
						{ID: "cbx_retained", Provider: "aws", State: "released", Keep: true},
						{ID: "cbx_ended", Provider: "aws", State: "expired"},
						{ID: "cbx_deleted", Provider: "aws", State: "released", Keep: true, CleanupStatus: "complete", CleanupCompletedAt: "2026-01-01T00:00:00Z"},
					}})
				}))
				defer server.Close()
				listTestConfig(t, server.URL)
				var stdout, stderr bytes.Buffer
				args := []string{"--provider", "aws"}
				if retained {
					args = append(args, "--retained")
				}
				if format != "" {
					args = append(args, format)
				}
				if err := (App{Stdout: &stdout, Stderr: &stderr}).list(t.Context(), args); err != nil {
					t.Fatal(err)
				}
				got := stdout.String()
				if !strings.Contains(got, "cbx_active") || !strings.Contains(got, "cbx_pending") || strings.Contains(got, "cbx_retained") != retained || strings.Contains(got, "cbx_ended") || strings.Contains(got, "cbx_deleted") {
					t.Fatalf("stdout=%s stderr=%s", got, &stderr)
				}
			})
		}
	}
}

func TestListStreamingRetainedHintUsesStdoutTTY(t *testing.T) {
	for _, stdoutTTY := range []bool{false, true} {
		for _, stderrTTY := range []bool{false, true} {
			for _, option := range []string{"", "--retained", "--json", "--jsonl"} {
				t.Run(fmt.Sprintf("stdout=%t/stderr=%t/%s", stdoutTTY, stderrTTY, option), func(t *testing.T) {
					var stdout bytes.Buffer
					stderr := listObservedWriter{needle: "unused", written: make(chan struct{})}
					original := listIsTerminal
					listIsTerminal = func(w io.Writer) bool { return w == &stdout && stdoutTTY || w == &stderr && stderrTTY }
					t.Cleanup(func() { listIsTerminal = original })
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if strings.Contains(stderr.String(), "retained leases hidden") {
							t.Error("hint emitted before live requests finished")
						}
						state := r.URL.Query().Get("state")
						_ = json.NewEncoder(w).Encode(map[string]any{"pagination": "keyset-v1", "leases": []CoordinatorLease{{ID: "cbx_" + state, Provider: "aws", State: state}}})
					}))
					defer server.Close()
					listTestConfig(t, server.URL)
					args := []string{"--provider", "aws"}
					if option != "" {
						args = append(args, option)
					}
					if err := (App{Stdout: &stdout, Stderr: &stderr}).list(t.Context(), args); err != nil {
						t.Fatal(err)
					}
					const hint = "\x1b[2mretained leases hidden; use --retained to include them\x1b[0m\n"
					want := stdoutTTY && option == ""
					if strings.Contains(stderr.String(), hint) != want || strings.Contains(stdout.String(), "retained leases hidden") || strings.Count(stderr.String(), hint) > 1 {
						t.Fatalf("stdout=%q stderr=%q wantHint=%t", &stdout, &stderr, want)
					}
				})
			}
		}
	}
}

func TestListStreamingLiveFlagRemoved(t *testing.T) {
	listTestConfig(t, "https://unused.example.test")
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).list(t.Context(), []string{"--live"})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -live") || stdout.Len() != 0 {
		t.Fatalf("err=%v stdout=%s stderr=%s", err, &stdout, &stderr)
	}
}
