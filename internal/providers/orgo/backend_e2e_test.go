package orgo

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestOrgoBackendEndToEndCreateRunDelete drives the real *orgoHTTPClient through
// backend.Run against a fake Orgo REST API, exercising the full delegated-run
// lifecycle: create workspace -> create computer -> run bash -> delete computer
// -> delete workspace.
//
// No real secrets: the API key is a dummy value supplied via
// CRABBOX_ORGO_API_KEY and the resolved base URL is the in-process httptest
// server, so the test never reaches the live Orgo API.
func TestOrgoBackendEndToEndCreateRunDelete(t *testing.T) {
	// Isolate the on-disk lease claim so the test never touches real state.
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const (
		dummyKey    = "test-key"
		workspaceID = "ws_e2e"
		computerID  = "computer_e2e"
	)

	var (
		mu  sync.Mutex
		hit = map[string]bool{}
	)
	mark := func(k string) { mu.Lock(); hit[k] = true; mu.Unlock() }

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+dummyKey {
			t.Errorf("authorization=%q, want Bearer %s", got, dummyKey)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/workspaces":
			mark("create_workspace")
			_ = json.NewEncoder(w).Encode(orgoWorkspace{ID: workspaceID, Name: "crabbox-e2e", Status: "active"})
		case r.Method == http.MethodPost && r.URL.Path == "/computers":
			mark("create_computer")
			var req orgoCreateComputerRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.WorkspaceID != workspaceID {
				t.Errorf("create computer workspace_id=%q, want %q", req.WorkspaceID, workspaceID)
			}
			_ = json.NewEncoder(w).Encode(orgoComputer{
				ID: computerID, InstanceID: "instance_e2e", Name: req.Name,
				WorkspaceID: req.WorkspaceID, Status: "running",
				ConnectionURL: "https://www.orgo.ai/desktops/instance_e2e",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/computers/"+computerID+"/bash":
			mark("run_bash")
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			if cmd, _ := req["command"].(string); !strings.Contains(cmd, "crabbox-orgo-ok") {
				t.Errorf("bash command=%q, want it to contain crabbox-orgo-ok", req["command"])
			}
			zero := 0
			_ = json.NewEncoder(w).Encode(orgoBashResponse{Stdout: "crabbox-orgo-ok\n", ExitCodeCamel: &zero})
		case r.Method == http.MethodDelete && r.URL.Path == "/computers/"+computerID:
			mark("delete_computer")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/workspaces/"+workspaceID:
			mark("delete_workspace")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	// Dummy key + resolved fake base URL; the production client is routed at the test server.
	t.Setenv("CRABBOX_ORGO_API_KEY", dummyKey)

	var out, errBuf bytes.Buffer
	backend := NewOrgoBackend(Provider{}.Spec(), Config{Orgo: OrgoConfig{APIBase: server.URL}}, Runtime{
		Stdout: &out, Stderr: &errBuf, HTTP: server.Client(),
	}).(*orgoBackend)

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: t.TempDir()},
		NoSync:  true,
		Command: []string{"echo", "crabbox-orgo-ok"},
	})
	if err != nil {
		t.Fatalf("Run: %v (stderr=%q)", err, errBuf.String())
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode=%d, want 0 (result=%#v)", result.ExitCode, result)
	}
	if !result.SyncDelegated {
		t.Fatalf("SyncDelegated=false, want true for a delegated-run provider (result=%#v)", result)
	}
	if !strings.Contains(out.String(), "crabbox-orgo-ok") {
		t.Fatalf("stdout=%q, want it to contain crabbox-orgo-ok", out.String())
	}
	for _, k := range []string{"create_workspace", "create_computer", "run_bash", "delete_computer", "delete_workspace"} {
		if !hit[k] {
			t.Fatalf("missing expected Orgo API call %q (hits=%v)", k, hit)
		}
	}
}
