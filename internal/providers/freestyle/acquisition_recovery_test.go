package freestyle

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestFreestyleResizeFailureUsesAcquisitionRecovery(t *testing.T) {
	for _, deleteFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("delete failure=%t", deleteFails), func(t *testing.T) { testFreestyleResizeFailureRecovery(t, deleteFails) })
	}
}

func testFreestyleResizeFailureRecovery(t *testing.T, deleteFails bool) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const id = "vm-resize-recovery"
	deleted := false
	deleteCalls, execCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /v5/vms":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"vm-resize-recovery","displayName":"crabbox-repo-abc123","state":"running"}`)
		case "POST /v5/vms/" + id + "/resize":
			http.Error(w, "resize rejected", http.StatusForbidden)
		case "GET /v5/vms/" + id:
			io.WriteString(w, `{"id":"vm-resize-recovery","displayName":"crabbox-repo-abc123","state":"running"}`)
		case "DELETE /v5/vms/" + id:
			deleteCalls++
			if deleteFails {
				http.Error(w, "temporary delete failure", http.StatusServiceUnavailable)
				return
			}
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		case "POST /v5/vms/" + id + "/exec-await":
			execCalls++
			io.WriteString(w, `{"statusCode":0,"stdout":"","stderr":""}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var stderr bytes.Buffer
	backend := &freestyleBackend{
		spec: Provider{}.Spec(),
		cfg:  core.Config{Freestyle: core.FreestyleConfig{APIKey: "test-key", APIURL: server.URL, VCPUs: 8}},
		rt:   core.Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: &stderr},
	}
	client := &freestyleHTTPClient{apiKey: "test-key", apiURL: server.URL, httpClient: server.Client()}
	repo := core.Repo{Root: t.TempDir(), Name: "repo"}
	_, _, _, err := backend.createSandbox(t.Context(), client, repo, false, "")
	if err == nil {
		t.Fatal("expected resize failure")
	}
	if !strings.Contains(err.Error(), "resize rejected") {
		t.Fatalf("original error lost: %v", err)
	}
	if _, found, err := core.ReadLeaseClaimWithPresence("fsb_" + id); err != nil || found {
		t.Fatalf("unexpected claim: found=%t err=%v", found, err)
	}
	if !deleteFails {
		if deleteCalls != 1 || !deleted || execCalls != 0 {
			t.Fatalf("rollback deleteCalls=%d deleted=%t execCalls=%d", deleteCalls, deleted, execCalls)
		}
		return
	}
	for _, want := range []string{
		"resize rejected", "temporary delete failure",
		"crabbox run --provider freestyle --id 'fsb_vm-resize-recovery' --reclaim --no-sync -- true",
		"crabbox stop --provider freestyle --id 'fsb_vm-resize-recovery'",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing recovery detail %q in %v", want, err)
		}
	}
	if deleteCalls != 1 || execCalls != 0 || deleted {
		t.Fatalf("deleteCalls=%d execCalls=%d deleted=%t", deleteCalls, execCalls, deleted)
	}
	if _, found, err := core.ReadLeaseClaimWithPresence("fsb_" + id); err != nil || found {
		t.Fatalf("unexpected claim: found=%t err=%v", found, err)
	}
	if err := backend.Stop(t.Context(), core.StopRequest{ID: "fsb_" + id}); err == nil {
		t.Fatal("ordinary stop must require an exact claim")
	}
	if deleteCalls != 1 {
		t.Fatal("unclaimed stop reached delete")
	}
	if t.Failed() {
		t.FailNow()
	}
	result, err := backend.Run(t.Context(), core.RunRequest{ID: "fsb_" + id, Repo: repo, Reclaim: true, NoSync: true, Command: []string{"true"}})
	if err != nil || result.Session == nil || !result.Session.Kept {
		t.Fatalf("reclaim result=%+v err=%v", result, err)
	}
	if _, found, err := core.ReadLeaseClaimWithPresence("fsb_" + id); err != nil || !found {
		t.Fatalf("adoption claim: found=%t err=%v", found, err)
	}
	deleteFails = false
	if err := backend.Stop(t.Context(), core.StopRequest{ID: "fsb_" + id}); err != nil {
		t.Fatal(err)
	}
	if !deleted || deleteCalls != 2 {
		t.Fatalf("recovery cleanup: deleted=%t calls=%d", deleted, deleteCalls)
	}
	if _, found, err := core.ReadLeaseClaimWithPresence("fsb_" + id); err != nil || found {
		t.Fatalf("deleted claim retained: found=%t err=%v", found, err)
	}
}
