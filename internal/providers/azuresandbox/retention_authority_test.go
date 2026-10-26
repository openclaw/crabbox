package azuresandbox

import (
	core "github.com/openclaw/crabbox/internal/cli"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// This exercises the production client over loopback HTTP, not Azure service
// qualification. The service state and credential remain explicitly synthetic.
func TestRetainedPendingAuthorityOverRealHTTP(t *testing.T) {
	for _, action := range []string{"pause", "resume"} {
		t.Run(action, func(t *testing.T) {
			b, req, f, _, _ := retainedFixture(t)
			synthetic := b.api.http.Transport
			var mu sync.Mutex
			requests, effects := 0, 0
			failNext := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				requests++
				if r.Method != http.MethodGet {
					effects++
				}
				if failNext && r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/stop") || strings.HasSuffix(r.URL.Path, "/resume")) {
					failNext = false
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				response, err := synthetic.RoundTrip(r)
				if err != nil {
					t.Errorf("service fixture: %v", err)
					w.WriteHeader(500)
					return
				}
				defer response.Body.Close()
				w.WriteHeader(response.StatusCode)
				_, _ = io.Copy(w, response.Body)
			}))
			defer server.Close()
			b.api.endpoint = server.URL
			b.api.http.Transport = nil
			if _, err := b.acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			invoke := func(action string, request core.RetentionLifecycleRequest) error {
				if action == "pause" {
					return b.Pause(t.Context(), core.PauseRequest{ID: req.RequestedLeaseID, Lifecycle: request})
				}
				return b.Resume(t.Context(), core.ResumeRequest{ID: req.RequestedLeaseID, Lifecycle: request})
			}
			if action == "resume" {
				setup := retentionRequest(t, b, req.RequestedLeaseID, "setup-pause")
				if err := invoke("pause", setup); err != nil {
					t.Fatal(err)
				}
			}
			request := retentionRequest(t, b, req.RequestedLeaseID, "pending-"+action)
			mu.Lock()
			failNext = true
			mu.Unlock()
			if err := invoke(action, request); err == nil {
				t.Fatal("uncertain lifecycle response succeeded")
			}
			pending, err := b.claim(req.RequestedLeaseID)
			if err != nil {
				t.Fatal(err)
			}
			window := pending.FixedCreateIntent.Journal.Retention.ComputeWindow
			if pending.FixedCreateIntent.Journal.Retention.Operation.Phase != "pending" {
				t.Fatal("pending operation not durable")
			}
			mu.Lock()
			originalAttempt := f.box.Labels["crabbox_attempt"]
			f.box.Labels["crabbox_attempt"] = "foreign-attempt"
			beforeEffects := effects
			mu.Unlock()
			if err := invoke(action, request); err == nil {
				t.Fatal("foreign resource accepted pending replay")
			}
			if _, err := b.Run(t.Context(), core.RunRequest{ID: req.RequestedLeaseID, Repo: req.Repo, Keep: true, NoSync: true, Command: []string{"true"}}); err == nil {
				t.Fatal("pending work admitted")
			}
			if _, err := b.Heartbeat(t.Context(), core.LeaseHeartbeatRequest{ID: req.RequestedLeaseID}); err == nil {
				t.Fatal("pending heartbeat admitted")
			}
			source := filepath.Join(t.TempDir(), "input")
			if err := os.WriteFile(source, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := b.Copy(t.Context(), core.CopyRequest{ID: req.RequestedLeaseID, RepoRoot: req.Repo.Root, Source: source, Destination: "SANDBOX:/workspace/input"}); err == nil {
				t.Fatal("pending copy admitted")
			}
			if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err == nil {
				t.Fatal("foreign resource deleted")
			}
			mu.Lock()
			afterEffects := effects
			beforeRequests := requests
			mu.Unlock()
			if afterEffects != beforeEffects {
				t.Fatal("foreign authority reached a provider effect")
			}
			foreign := request
			foreign.OwnerBinding = strings.Repeat("b", 64)
			if err := invoke(action, foreign); err == nil {
				t.Fatal("foreign owner accepted")
			}
			mu.Lock()
			afterRequests := requests
			f.box.Labels["crabbox_attempt"] = originalAttempt
			mu.Unlock()
			if afterRequests != beforeRequests {
				t.Fatal("foreign owner reached provider I/O")
			}
			if err := invoke(action, request); err != nil {
				t.Fatalf("exact pending recovery failed: %v", err)
			}
			recovered, err := b.claim(req.RequestedLeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.FixedCreateIntent.Journal.Retention.ComputeWindow != window {
				t.Fatal("recovery renewed work window")
			}
			successor := "resume"
			if action == "resume" {
				successor = "pause"
			}
			next := retentionRequest(t, b, req.RequestedLeaseID, "successor-"+successor)
			if err := invoke(successor, next); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			beforeRequests = requests
			mu.Unlock()
			if err := invoke(action, request); err == nil {
				t.Fatal("superseded operation accepted")
			}
			mu.Lock()
			afterRequests = requests
			mu.Unlock()
			if afterRequests != beforeRequests {
				t.Fatal("superseded request reached provider I/O")
			}
		})
	}
}
