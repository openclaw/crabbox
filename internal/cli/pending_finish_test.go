package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type pendingFinishFixture struct {
	t           *testing.T
	mu          sync.Mutex
	server      *httptest.Server
	client      *CoordinatorClient
	pending     pendingRunFinish
	receipt     terminalRunReceipt
	run         CoordinatorRun
	who         CoordinatorWhoami
	log         string
	posts       int
	commits     int
	requests    int
	committed   []byte
	failPost    bool
	conflict    bool
	badReadback bool
}

func newPendingFinishFixture(t *testing.T) *pendingFinishFixture {
	t.Helper()
	setAttestTestHome(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	f := &pendingFinishFixture{t: t, log: "original task output\n", who: CoordinatorWhoami{Owner: "alice@example.com", Org: "example-org"}}
	started := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	f.run = CoordinatorRun{ID: "run_123", Owner: f.who.Owner, Org: f.who.Org, LeaseID: "cbx_abc123", Slug: "blue-lobster", Provider: "aws", Command: []string{"false"}, StartedAt: started.Format(time.RFC3339Nano), State: "running"}
	var err error
	f.receipt, err = buildTerminalRunReceipt("", terminalRunReceiptInput{Provider: f.run.Provider, LeaseID: f.run.LeaseID, Slug: f.run.Slug, RunID: f.run.ID, Command: f.run.Command, CommandDisplay: "false", ExitCode: 1, SyncMs: 100, CommandMs: 1900, StartedAt: started, EndedAt: started.Add(2 * time.Second), LogSHA256: sha256Digest([]byte(f.log)), RetainedLogSHA256: sha256Digest([]byte(f.log))})
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	f.client = &CoordinatorClient{BaseURL: f.server.URL, Client: f.server.Client()}
	body, err := encodeRunFinish(1, 100*time.Millisecond, 1900*time.Millisecond, f.log, false, nil, nil, FailureClassification{}, &f.receipt)
	if err != nil {
		t.Fatal(err)
	}
	// Whitespace is intentionally noncanonical: import must not re-marshal it.
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, body, "", "  "); err != nil {
		t.Fatal(err)
	}
	body = append(formatted.Bytes(), '\n')
	f.pending = pendingRunFinish{SchemaVersion: 1, Coordinator: f.server.URL, RunID: f.run.ID, Owner: f.run.Owner, Org: f.run.Org, Body: body, BodySHA256: sha256Digest(body)}
	return f
}

func (f *pendingFinishFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	switch r.Method + " " + r.URL.Path {
	case "GET /v1/whoami":
		_ = json.NewEncoder(w).Encode(f.who)
	case "GET /v1/runs/run_123":
		_ = json.NewEncoder(w).Encode(map[string]any{"run": f.run})
	case "POST /v1/runs/run_123/finish":
		f.posts++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			f.t.Error(err)
			http.Error(w, "read", 400)
			return
		}
		if !bytes.Equal(body, f.pending.Body) {
			f.t.Error("submitted finish bytes changed")
		}
		if f.conflict || (len(f.committed) > 0 && !bytes.Equal(body, f.committed)) {
			http.Error(w, "terminal_run_conflict", 409)
			return
		}
		if len(f.committed) == 0 {
			f.commits++
			f.committed = bytes.Clone(body)
			f.run.State = "failed"
			f.run.ExitCode = intPointer(1)
			f.run.SyncMs = 100
			f.run.CommandMs = 1900
			f.run.EndedAt = f.receipt.EndedAt
		}
		if f.failPost {
			http.Error(w, "response unavailable", 503)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"run": f.run})
	case "GET /v1/runs/run_123/receipt":
		if len(f.committed) == 0 {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"receipt": f.receipt})
	case "GET /v1/runs/run_123/logs":
		if f.badReadback {
			_, _ = io.WriteString(w, "not the original log")
			return
		}
		_, _ = io.WriteString(w, f.log)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func (f *pendingFinishFixture) retain() string {
	f.t.Helper()
	path, err := retainPendingRunFinish(f.pending)
	if err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *pendingFinishFixture) recover(path string) (terminalRunReceipt, error) {
	return recoverPendingRunFinish(context.Background(), f.client, f.pending.RunID, path, f.receipt.Signer, f.pending.BodySHA256)
}

func TestPendingFinishRecoveryPreservesBytesAndIsIdempotent(t *testing.T) {
	f := newPendingFinishFixture(t)
	path := f.retain()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	keyPath, err := attestKeyPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		receipt, err := f.recover(path)
		if err != nil {
			t.Fatal(err)
		}
		if receipt != f.receipt {
			t.Fatal("original receipt changed")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("import modified original evidence")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 2 || f.commits != 1 {
		t.Fatalf("posts=%d commits=%d", f.posts, f.commits)
	}
}

func TestPendingFinishRecoveryRequiresOriginalTrustedBindings(t *testing.T) {
	for _, name := range []string{"signer", "digest", "missing-signer", "missing-digest", "metadata", "coordinator", "run", "missing-owner", "missing-org", "signature", "log", "exit", "duplicate", "unknown", "trailing", "bare-receipt"} {
		t.Run(name, func(t *testing.T) {
			f := newPendingFinishFixture(t)
			path := f.retain()
			signer, digest, runID := f.receipt.Signer, f.pending.BodySHA256, f.pending.RunID
			switch name {
			case "signer":
				signer = "sha256:" + strings.Repeat("0", 64)
			case "digest":
				digest = "sha256:" + strings.Repeat("0", 64)
			case "missing-signer":
				signer = ""
			case "missing-digest":
				digest = ""
			case "coordinator":
				f.client.BaseURL = "https://other.invalid"
			case "run":
				runID = "run_other"
			default:
				p := f.pending
				switch name {
				case "missing-owner":
					p.Owner = ""
				case "missing-org":
					p.Org = ""
				case "signature", "log", "exit", "metadata":
					var body map[string]any
					if err := json.Unmarshal(p.Body, &body); err != nil {
						t.Fatal(err)
					}
					switch name {
					case "signature":
						body["receipt"].(map[string]any)["signature"] = "invalid"
					case "log":
						body["logChunks"] = []string{"different"}
					case "exit":
						body["exitCode"] = 0
					case "metadata":
						// Schema-v2 receipt signatures do not bind these fields.
						body["telemetry"] = map[string]any{"changed": true}
					}
					p.Body, _ = json.Marshal(body)
					p.BodySHA256 = sha256Digest(p.Body)
					if name != "metadata" {
						digest = p.BodySHA256
					}
				}
				data, _ := json.Marshal(p)
				switch name {
				case "duplicate":
					data = append([]byte("{\"run_id\":\"run_123\","), data[1:]...)
				case "unknown":
					data = append([]byte("{\"unrecognized\":true,"), data[1:]...)
				case "trailing":
					data = append(data, []byte(" {}")...)
				case "bare-receipt":
					data, _ = json.Marshal(f.receipt)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := recoverPendingRunFinish(context.Background(), f.client, runID, path, signer, digest)
			if err == nil {
				t.Fatal("invalid binding accepted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.requests != 0 {
				t.Fatalf("invalid input reached coordinator: %d requests", f.requests)
			}
		})
	}
}

func TestPendingFinishRecoveryRejectsOwnerAndRunChangesBeforePost(t *testing.T) {
	for _, name := range []string{"caller-owner", "caller-org", "admin-other-owner", "run-owner", "run-org", "lease", "provider", "command", "start", "slug", "run-id"} {
		t.Run(name, func(t *testing.T) {
			f := newPendingFinishFixture(t)
			path := f.retain()
			switch name {
			case "caller-owner":
				f.who.Owner = "bob@example.com"
			case "caller-org":
				f.who.Org = "other-org"
			case "admin-other-owner":
				f.who.Owner = "bob@example.com"
				f.who.Admin = true
			case "run-owner":
				f.run.Owner = "bob@example.com"
			case "run-org":
				f.run.Org = "other-org"
			case "lease":
				f.run.LeaseID = "cbx_other"
			case "provider":
				f.run.Provider = "hetzner"
			case "command":
				f.run.Command = []string{"true"}
			case "start":
				f.run.StartedAt = "2026-08-23T10:00:01Z"
			case "slug":
				f.run.Slug = "other"
			case "run-id":
				f.run.ID = "run_other"
			}
			if _, err := f.recover(path); err == nil {
				t.Fatal("changed identity accepted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.posts != 0 {
				t.Fatalf("posted changed run: %d", f.posts)
			}
		})
	}
}

func TestPendingFinishRecoveryDoesNotInferCommitFromTransportFailure(t *testing.T) {
	f := newPendingFinishFixture(t)
	path := f.retain()
	f.failPost = true
	if _, err := f.recover(path); err == nil {
		t.Fatal("failed submission reported success")
	}
	f.mu.Lock()
	if f.posts != 1 {
		t.Fatalf("implicit retries: %d", f.posts)
	}
	f.failPost = false
	f.mu.Unlock()
	got, err := f.recover(path)
	if err != nil || got != f.receipt {
		t.Fatalf("explicit retry receipt=%+v error=%v", got, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commits != 1 {
		t.Fatalf("duplicate commits: %d", f.commits)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("original lost", err)
	}
}

func TestPendingFinishRecoveryRequiresCommitAndReadback(t *testing.T) {
	for _, name := range []string{"conflict", "log"} {
		t.Run(name, func(t *testing.T) {
			f := newPendingFinishFixture(t)
			path := f.retain()
			if _, err := f.recover(path); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.conflict = name == "conflict"
			f.badReadback = name == "log"
			f.mu.Unlock()
			if _, err := f.recover(path); err == nil {
				t.Fatal("unconfirmed or conflicting terminal accepted")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("original lost", err)
			}
		})
	}
}

func TestPendingFinishRetentionIsPrivateAndNoClobber(t *testing.T) {
	f := newPendingFinishFixture(t)
	path := f.retain()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, filepath.Dir(path)} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0 {
			t.Fatalf("public permissions: %s %v", p, st.Mode())
		}
	}
	if same := f.retain(); same != path {
		t.Fatal("unstable outbox key")
	}
	conflicting := f.pending
	// These unsigned metadata bytes still belong to the pinned original request.
	conflicting.Body = append(bytes.Clone(conflicting.Body), ' ')
	conflicting.BodySHA256 = sha256Digest(conflicting.Body)
	if _, err := retainPendingRunFinish(conflicting); err == nil {
		t.Fatal("conflicting original overwrote outbox")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("outbox changed")
	}
	if runtime.GOOS == "windows" {
		return
	} // ACL denial is covered by the Windows test.
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.recover(path); err == nil {
		t.Fatal("public import accepted")
	}
}

func TestPendingFinishRecoveryRefusesSymlink(t *testing.T) {
	f := newPendingFinishFixture(t)
	path := f.retain()
	link := filepath.Join(t.TempDir(), "finish.json")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := f.recover(link); err == nil {
		t.Fatal("symlink import accepted")
	}
}

func TestRunRecorderRetainsOriginalBeforePostAndAfterFailure(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusConflict} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			receipt := runRecorderTestReceipt(t)
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			var server *httptest.Server
			var pendingPath string
			var original []byte
			posts := 0
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/runs/run_123/finish":
					posts++
					path, err := pendingRunFinishPath(pendingRunFinish{Coordinator: server.URL, RunID: "run_123"})
					if err != nil {
						t.Error(err)
						return
					}
					pendingPath = path
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Error("not retained before POST", err)
						return
					}
					var p pendingRunFinish
					if err := decodePendingFinishJSON(raw, &p); err != nil {
						t.Error(err)
						return
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					if !bytes.Equal(body, p.Body) {
						t.Error("stored bytes differ from posted bytes")
					}
					if posts == 1 {
						original = raw
					} else if !bytes.Equal(original, raw) {
						t.Error("outbox changed during retry")
					}
					http.Error(w, "not confirmed", status)
				case "/v1/runs/run_123/receipt":
					// A matching receipt must not turn a conflicting full finish into success.
					if status == http.StatusConflict {
						_ = json.NewEncoder(w).Encode(map[string]any{"receipt": receipt})
					} else {
						http.NotFound(w, r)
					}
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			var stderr bytes.Buffer
			rec := &runRecorder{coord: &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}, runID: "run_123", runOwner: "alice@example.com", runOrg: "example-org", stderr: &stderr}
			finishErr := rec.Finish(context.Background(), SSHTarget{}, 1, 0, 100*time.Millisecond, "", false, nil, FailureClassification{}, &receipt)
			if finishErr == nil {
				t.Fatal("failed finish accepted")
			}
			if stderr.Len() != 0 {
				t.Fatalf("finish appended progress output after final timing: %s", stderr.String())
			}
			if rec.terminalConfirmed {
				t.Fatal("ambiguous finish marked confirmed")
			}
			if status == http.StatusConflict && posts != 1 {
				t.Fatalf("conflict retried %d times", posts)
			}
			after, err := os.ReadFile(pendingPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, after) {
				t.Fatal("failed finish evidence changed")
			}
			var pending pendingRunFinish
			if err := decodePendingFinishJSON(after, &pending); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"run_123", pendingPath, receipt.Signer, sha256Digest(pending.Body), "receipt --recover"} {
				if !strings.Contains(finishErr.Error(), want) {
					t.Fatalf("terminal error lost recovery detail %q: %v", want, finishErr)
				}
			}
		})
	}
}

func TestRunRecorderDoesNotPostWhenRetentionFails(t *testing.T) {
	receipt := runRecorderTestReceipt(t)
	state := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(state, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", state)
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts++; http.Error(w, "unexpected", 500) }))
	defer server.Close()
	rec := &runRecorder{coord: &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}, runID: "run_123", stderr: io.Discard}
	err := rec.Finish(context.Background(), SSHTarget{}, 1, 0, 100*time.Millisecond, "", false, nil, FailureClassification{}, &receipt)
	if err == nil || !strings.Contains(err.Error(), "retain original") {
		t.Fatalf("error=%v", err)
	}
	if posts != 0 {
		t.Fatalf("sent %d requests without retained original", posts)
	}
}

func TestPendingFinishRecoveryPinsAuthentication(t *testing.T) {
	f := newPendingFinishFixture(t)
	path := f.retain()
	f.client.Token = "original-token"
	transport := f.server.Client().Transport
	calls := 0
	f.client.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if got := req.Header.Get("Authorization"); got != "Bearer original-token" {
			t.Errorf("authentication changed: %q", got)
		}
		f.client.Token = "changed-token"
		return transport.RoundTrip(req)
	})}
	if _, err := f.recover(path); err != nil {
		t.Fatal(err)
	}
	if calls < 5 {
		t.Fatalf("missing bound readback requests: %d", calls)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("original-token")) || bytes.Contains(data, []byte("changed-token")) {
		t.Fatal("authentication persisted in outbox")
	}
}

func TestPendingFinishBoundTransportDoesNotReauthenticateThroughCurl(t *testing.T) {
	original := errors.New("original transport failure")
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer original-token")
	client := &CoordinatorClient{BaseURL: "https://coordinator.invalid", Token: "changed-token", admissionAuth: &coordinatorAdmissionAuth{captured: true, headers: headers}, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer original-token" {
			t.Error("lost bound identity")
		}
		return nil, original
	})}}
	var response any
	err := client.do(context.Background(), http.MethodGet, "/v1/whoami", nil, &response)
	if !errors.Is(err, original) || strings.Contains(err.Error(), "curl fallback") {
		t.Fatalf("bound operation used another transport: %v", err)
	}
}

func TestReceiptCommandImportsOriginalFinish(t *testing.T) {
	f := newPendingFinishFixture(t)
	path := f.retain()
	t.Setenv("CRABBOX_COORDINATOR", f.server.URL)
	var out, stderr bytes.Buffer
	app := App{Stdout: &out, Stderr: &stderr}
	if err := app.receipt(context.Background(), []string{f.pending.RunID, "--recover", path, "--expected-signer", f.receipt.Signer, "--expected-finish-sha256", f.pending.BodySHA256}); err != nil {
		t.Fatal(err)
	}
	receipt, err := decodeTerminalRunReceipt(out.Bytes())
	if err != nil || receipt != f.receipt {
		t.Fatalf("receipt=%+v error=%v", receipt, err)
	}
}

func TestRunRecorderFinishUsesOriginalAdmissionClient(t *testing.T) {
	receipt := runRecorderTestReceipt(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer original-token" {
			t.Error("finish changed admission identity")
		}
		if r.URL.Path == "/v1/runs/run_123/receipt" {
			_ = json.NewEncoder(w).Encode(map[string]any{"receipt": receipt})
			return
		}
		if r.URL.Path != "/v1/runs/run_123/finish" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"run": CoordinatorRun{ID: "run_123"}})
	}))
	defer server.Close()
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer original-token")
	rec := &runRecorder{coord: &CoordinatorClient{BaseURL: "https://other.invalid", Token: "changed-token"}, createCoordinator: &CoordinatorClient{BaseURL: server.URL, Client: server.Client(), admissionAuth: &coordinatorAdmissionAuth{captured: true, headers: headers}}, runID: "run_123", runOwner: "alice@example.com", runOrg: "example-org", stderr: io.Discard}
	if err := rec.Finish(context.Background(), SSHTarget{}, 1, 0, 100*time.Millisecond, "", false, nil, FailureClassification{}, &receipt); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests=%d", requests)
	}
	path, err := pendingRunFinishPath(pendingRunFinish{Coordinator: server.URL, RunID: "run_123"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("confirmed finish left pending file: %v", err)
	}
}
