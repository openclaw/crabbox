package cli

import (
	"bytes"
	"context"
	"encoding/json"
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

// This exercises a real token-command subprocess and wall-clock expiry against
// a local HTTP fixture. It is not proof of coordinator authentication/storage.
func TestRunRecorderFinishRefreshesExpiringToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("token-command fixture uses POSIX sh")
	}
	for _, tc := range []struct {
		name                          string
		owner, org                    string
		admin                         bool
		runOwner, runOrg              string
		retry, revoked, helperFailure bool
		wantError                     string
	}{
		{name: "same-owner-org"},
		{name: "same-owner-org-retry", retry: true},
		{name: "other-owner", owner: "bob@example.com", wantError: "owner or organization mismatch"},
		{name: "other-org", org: "other-org", wantError: "owner or organization mismatch"},
		{name: "admin-other-owner", owner: "bob@example.com", admin: true, wantError: "owner or organization mismatch"},
		{name: "admin-other-org", org: "other-org", admin: true, wantError: "owner or organization mismatch"},
		{name: "stored-owner-changed", runOwner: "bob@example.com", wantError: "run owner or organization mismatch"},
		{name: "stored-org-changed", runOrg: "other-org", wantError: "run owner or organization mismatch"},
		{name: "revoked-before-post", revoked: true, wantError: "401"},
		{name: "helper-failure", helperFailure: true, wantError: "token command failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receipt := runRecorderTestReceipt(t)
			dir := t.TempDir()
			tokenFile, callsFile := filepath.Join(dir, "token"), filepath.Join(dir, "calls")
			if err := os.WriteFile(tokenFile, []byte("admitted-token"), 0600); err != nil {
				t.Fatal(err)
			}
			helper := []string{"sh", "-c", "printf x >> \"$1\"; cat \"$2\"", "token-helper", callsFile, tokenFile}
			var mu sync.Mutex
			var expires time.Time
			posts, commits, reads := 0, 0, 0
			revoked := false
			var pendingBefore, posted []byte
			run := CoordinatorRun{ID: "run_123", Owner: "alice@example.com", Org: "example-org", Command: []string{"false"}, State: "running", Phase: "starting", StartedAt: receipt.StartedAt}
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
				if req.Method == http.MethodPut && req.URL.Path == "/v1/runs/run_123" {
					if token != "admitted-token" {
						t.Error("wrong admission token")
					}
					expires = time.Now().Add(150 * time.Millisecond)
					_ = json.NewEncoder(w).Encode(map[string]any{"run": run})
					return
				}
				reads++
				validToken := token == "fresh-token" || (token == "admitted-token" && time.Now().Before(expires))
				if !validToken || revoked {
					http.Error(w, "expired or revoked", http.StatusUnauthorized)
					return
				}
				switch req.Method + " " + req.URL.Path {
				case "GET /v1/whoami":
					owner, org := run.Owner, run.Org
					if tc.owner != "" {
						owner = tc.owner
					}
					if tc.org != "" {
						org = tc.org
					}
					// Any subsequent helper invocation would change authority. Finish must use
					// the same fresh headers for inspection, retries and receipt readback.
					if err := os.WriteFile(tokenFile, []byte("substituted-token"), 0600); err != nil {
						t.Error(err)
					}
					_ = json.NewEncoder(w).Encode(CoordinatorWhoami{Owner: owner, Org: org, Admin: tc.admin})
				case "GET /v1/runs/run_123":
					inspected := run
					if tc.runOwner != "" {
						inspected.Owner = tc.runOwner
					}
					if tc.runOrg != "" {
						inspected.Org = tc.runOrg
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"run": inspected})
					revoked = tc.revoked
				case "POST /v1/runs/run_123/finish":
					posts++
					path, err := pendingRunFinishPath(pendingRunFinish{Coordinator: server.URL, RunID: run.ID})
					if err != nil {
						t.Error(err)
						http.Error(w, "path", 500)
						return
					}
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Error(err)
						http.Error(w, "retention", 500)
						return
					}
					var pending pendingRunFinish
					if err := decodePendingFinishJSON(raw, &pending); err != nil {
						t.Error(err)
						return
					}
					body, err := io.ReadAll(req.Body)
					if err != nil {
						t.Error(err)
						return
					}
					if !bytes.Equal(body, pending.Body) {
						t.Error("original finish bytes changed")
					}
					if posts == 1 {
						pendingBefore = raw
						posted = body
					} else if !bytes.Equal(pendingBefore, raw) || !bytes.Equal(posted, body) {
						t.Error("retry changed original")
					}
					if tc.retry && posts == 1 {
						http.Error(w, "transport unavailable", 503)
						return
					}
					commits++
					_ = json.NewEncoder(w).Encode(map[string]any{"run": run})
				case "GET /v1/runs/run_123/receipt":
					if commits == 0 {
						http.NotFound(w, req)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"receipt": receipt})
				default:
					t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
					http.NotFound(w, req)
				}
			}))
			defer server.Close()
			rec := &runRecorder{coord: &CoordinatorClient{BaseURL: server.URL, Client: server.Client(), TokenCommand: helper}, requestedRunID: run.ID, command: run.Command, stderr: io.Discard}
			if err := rec.createRun(context.Background(), "", Config{Provider: "aws"}); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			expiredAt := expires
			mu.Unlock()
			time.Sleep(time.Until(expiredAt) + 10*time.Millisecond)
			if tc.helperFailure {
				if err := os.Remove(tokenFile); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(tokenFile, []byte("fresh-token"), 0600); err != nil {
				t.Fatal(err)
			}
			// Later routing must not replace the initiating route/helper configuration.
			rec.coord = &CoordinatorClient{BaseURL: "https://other.invalid", Token: "substituted-token"}
			err := rec.Finish(context.Background(), SSHTarget{}, 1, 0, 100*time.Millisecond, "", false, nil, FailureClassification{}, &receipt)
			mu.Lock()
			defer mu.Unlock()
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("finish after admission token expiry: %v (posts=%d commits=%d)", err, posts, commits)
				}
				expectedPosts := 1
				if tc.retry {
					expectedPosts = 2
				}
				if !rec.terminalConfirmed || commits != 1 || posts != expectedPosts {
					t.Fatalf("confirmed=%v posts=%d commits=%d", rec.terminalConfirmed, posts, commits)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error=%v want=%s", err, tc.wantError)
				}
				if rec.terminalConfirmed || commits != 0 || posts != 0 {
					t.Fatalf("unauthorized final effect: confirmed=%v posts=%d commits=%d", rec.terminalConfirmed, posts, commits)
				}
			}
			calls, readErr := os.ReadFile(callsFile)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(calls) != "xx" {
				t.Fatalf("helper calls=%d want admission+one finalization", len(calls))
			}
			if rec.createCoordinator.admissionAuth.headers.Get("Authorization") != "Bearer admitted-token" {
				t.Fatal("changed original admission binding")
			}
			path, pathErr := pendingRunFinishPath(pendingRunFinish{Coordinator: server.URL, RunID: run.ID})
			if pathErr != nil {
				t.Fatal(pathErr)
			}
			raw, readErr := os.ReadFile(path)
			if tc.wantError == "" {
				if !os.IsNotExist(readErr) {
					t.Fatalf("confirmed pending file: %v", readErr)
				}
			} else {
				if readErr != nil {
					t.Fatal("lost pending evidence", readErr)
				}
				var pending pendingRunFinish
				if err := decodePendingFinishJSON(raw, &pending); err != nil {
					t.Fatal(err)
				}
				_, retainedReceipt, validateErr := validatePendingRunFinish(pending)
				if validateErr != nil || retainedReceipt != receipt {
					t.Fatalf("original receipt changed: %v", validateErr)
				}
				if bytes.Contains(raw, []byte("fresh-token")) || bytes.Contains(raw, []byte("admitted-token")) {
					t.Fatal("persisted credentials")
				}
				for _, pin := range []string{path, receipt.Signer, pending.BodySHA256} {
					if !strings.Contains(err.Error(), pin) {
						t.Fatalf("error lost recovery pin %q: %v", pin, err)
					}
				}
			}
			t.Logf("helperCalls=%d HTTPAfterAdmission=%d authenticatedPOSTs=%d commits=%d confirmed=%v", len(calls), reads, posts, commits, rec.terminalConfirmed)
		})
	}
}
