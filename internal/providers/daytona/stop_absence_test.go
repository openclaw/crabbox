package daytona

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestDaytonaStopPreservesClaimOnUnverifiedAbsence(t *testing.T) {
	for _, tc := range []struct {
		name          string
		originalScope string
		listStatus    int
		disconnect    bool
		live          bool
		wantError     string
	}{
		{name: "missing resource", listStatus: 200, wantError: "bound to missing sandbox"},
		{name: "different endpoint", originalScope: "endpoint:https://original.example.test", listStatus: 200, wantError: "bound to missing sandbox"},
		{name: "different account", originalScope: "account:original-account", listStatus: 200, wantError: "bound to missing sandbox"},
		{name: "list authorization error", listStatus: 403, wantError: "list sandboxes: 403"},
		{name: "list service error", listStatus: 503, wantError: "list sandboxes: 503"},
		{name: "list transport error", disconnect: true, wantError: "list sandboxes"},
		{name: "exact owned deletion", listStatus: 200, live: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dirs := testutil.IsolateUserDirs(t)
			repo := t.TempDir()
			t.Chdir(repo)
			for _, name := range []string{"CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_MODE", "CRABBOX_COORDINATOR_TOKEN_COMMAND", "CRABBOX_POND", "CRABBOX_TAILSCALE", "CRABBOX_DAYTONA_JWT_TOKEN", "DAYTONA_JWT_TOKEN", "CRABBOX_DAYTONA_ORGANIZATION_ID", "DAYTONA_ORGANIZATION_ID"} {
				t.Setenv(name, "")
			}
			t.Setenv("CRABBOX_DAYTONA_API_KEY", "synthetic-current-account")
			const leaseID = "cbx_123456abcdef"
			const resourceID = "sandbox-original"
			labels := map[string]string{"crabbox": "true", "provider": "daytona", "lease": leaseID, "slug": "scoped-fixture"}
			var mu sync.Mutex
			gets, deletes := 0, 0
			state := "started"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				sandbox := map[string]any{
					"id": resourceID, "organizationId": "synthetic-current-account", "name": "scoped-fixture",
					"target": "us", "user": "daytona", "env": map[string]string{}, "public": false,
					"networkBlockAll": false, "cpu": 1, "gpu": 0, "memory": 1, "disk": 3,
					"toolboxProxyUrl": "https://toolbox.example.test/" + resourceID, "state": state, "labels": labels,
				}
				switch {
				case r.Method == "GET" && r.URL.Path == "/sandbox":
					if tc.disconnect {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
						return
					}
					w.WriteHeader(tc.listStatus)
					items := []any{}
					if tc.live {
						items = append(items, sandbox)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "nextCursor": nil})
				case r.Method == "GET" && r.URL.Path == "/sandbox/"+resourceID:
					gets++
					if !tc.live {
						w.WriteHeader(http.StatusNotFound)
						_, _ = fmt.Fprint(w, `{"message":"not visible in current account"}`)
						return
					}
					_ = json.NewEncoder(w).Encode(sandbox)
				case r.Method == "DELETE" && r.URL.Path == "/sandbox/"+resourceID:
					deletes++
					state = "destroyed"
					sandbox["state"] = state
					_ = json.NewEncoder(w).Encode(sandbox)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			t.Setenv("CRABBOX_DAYTONA_API_URL", srv.URL)
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(configPath, []byte("provider: daytona\ntarget: linux\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "scoped-fixture", "daytona", tc.originalScope, "", repo, time.Minute, false,
				core.Server{Provider: "daytona", CloudID: resourceID, Labels: labels}, core.SSHTarget{}); err != nil {
				t.Fatal(err)
			}
			claimPath := filepath.Join(dirs.StateHome, "crabbox", "claims", leaseID+".json")
			before, err := os.ReadFile(claimPath)
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			err = (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"stop", "--provider", "daytona", leaseID})
			mu.Lock()
			defer mu.Unlock()
			if tc.live {
				if err != nil || deletes != 1 || state != "destroyed" {
					t.Fatalf("err=%v deletes=%d state=%s, want confirmed exact deletion", err, deletes, state)
				}
				if _, err := os.Stat(claimPath); !os.IsNotExist(err) {
					t.Fatalf("claim remains after confirmed deletion: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("err=%v, want original resolution error containing %q", err, tc.wantError)
			}
			after, readErr := os.ReadFile(claimPath)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Errorf("claim changed after unverified absence: read error=%v", readErr)
			}
			if strings.Contains(stdout.String()+stderr.String(), "released") || deletes != 0 {
				t.Errorf("claimed release after unverified absence: deletes=%d stdout=%q stderr=%q", deletes, stdout.String(), stderr.String())
			}
			wantGets := 0
			if tc.listStatus == 200 {
				wantGets = 1
			}
			if gets != wantGets {
				t.Errorf("exact GET calls=%d, want %d without a second absence lookup", gets, wantGets)
			}
		})
	}
}
