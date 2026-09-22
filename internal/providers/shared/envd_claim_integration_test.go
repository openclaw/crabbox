package shared_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/cubesandbox"
	"github.com/openclaw/crabbox/internal/providers/e2b"
)

func TestEnvdClaimDeletionThroughProviderHTTP(t *testing.T) {
	for _, provider := range []core.Provider{e2b.Provider{}, cubesandbox.Provider{}} {
		for _, scenario := range []string{"deleted", "ownership changes inside fence", "delete fails"} {
			t.Run(provider.Spec().Name+"/"+scenario, func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				var mu sync.Mutex
				var metadata map[string]string
				var leaseID string
				var gets, deletes int
				present := false
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					switch {
					case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
						var input struct{ Metadata map[string]string }
						if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						metadata, leaseID, present = input.Metadata, input.Metadata["lease"], true
					case r.Method == http.MethodGet && r.URL.Path == "/sandboxes/sandbox-proof":
						gets++
						if scenario == "ownership changes inside fence" && gets == 2 {
							metadata["lease"] = "cbx_ffffffffffff"
						}
					case r.Method == http.MethodDelete && r.URL.Path == "/sandboxes/sandbox-proof":
						deletes++
						if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || !exists {
							t.Error("claim was removed before the real HTTP deletion")
						}
						if scenario == "delete fails" {
							http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
							return
						}
						present = false
						w.WriteHeader(http.StatusNoContent)
						return
					default:
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
						w.WriteHeader(500)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"sandboxID": "sandbox-proof", "templateID": "base", "state": "running", "metadata": metadata})
				}))
				defer server.Close()
				cfg := core.Config{
					Provider: provider.Spec().Name, IdleTimeout: time.Minute,
					E2B:         core.E2BConfig{APIURL: server.URL, APIKey: "synthetic-token", Template: "base", Workdir: "/workspace", User: "root"},
					CubeSandbox: core.CubeSandboxConfig{APIURL: server.URL, APIKey: "synthetic-token", Template: "base", Workdir: "/workspace", User: "root"},
				}
				configured, err := provider.Configure(cfg, core.Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
				if err != nil {
					t.Fatal(err)
				}
				backend := configured.(core.DelegatedRunBackend)
				if err := backend.Warmup(t.Context(), core.WarmupRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true}); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				id := leaseID
				mu.Unlock()
				before, err := core.ReadLeaseClaim(id)
				if err != nil || before.LeaseID == "" {
					t.Fatalf("warmup did not persist its real claim: %v", err)
				}
				stopErr := backend.Stop(t.Context(), core.StopRequest{ID: id})
				after, exists, readErr := core.ReadLeaseClaimWithPresence(id)
				mu.Lock()
				defer mu.Unlock()
				wantDeleted := scenario == "deleted"
				wantDeletes := 1
				if scenario == "ownership changes inside fence" {
					wantDeletes = 0
				}
				if (stopErr == nil) != wantDeleted || readErr != nil || present == wantDeleted || exists == wantDeleted || gets != 2 || deletes != wantDeletes {
					t.Fatalf("stop=%v read=%v resource=%t claim=%t GET=%d DELETE=%d", stopErr, readErr, present, exists, gets, deletes)
				}
				if !wantDeleted && !reflect.DeepEqual(after, before) {
					t.Fatal("rejected HTTP deletion changed the persisted claim")
				}
				t.Logf("production provider/HTTPS/claim store: GET=%d DELETE=%d resourcePresent=%t claimPresent=%t", gets, deletes, present, exists)
			})
		}
	}
}
