package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProjectCheckpointCLIRejectsUnboundedOrImplicitDiscardMetadata(t *testing.T) {
	for _, input := range []string{`{"action":"discard","generation":1}`, `{"action":"bind","content":"user bytes"}`, `{"action":"capture"} {}`, `{"action":"capture","acceptedRevision":"` + strings.Repeat("a", 16384) + `"}`} {
		var stdout, stderr bytes.Buffer
		app := App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(input)}
		if err := app.Run(context.Background(), []string{"project-checkpoint", "--id", "cbx_project", "--request-stdin", "--json"}); err == nil || stdout.Len() != 0 {
			t.Fatalf("invalid metadata reached coordinator: error=%v", err)
		}
	}
}

func TestProjectCheckpointCLITransportsOnlyExplicitBindingMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input ProjectCheckpointRequest
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/cbx_project/project-checkpoint" || json.NewDecoder(r.Body).Decode(&input) != nil || input.Action != "bind" || input.DependencyPolicy != "npm-lockfile-v1" || input.AcceptedRevision != "sha256:"+strings.Repeat("a", 64) {
			t.Errorf("binding metadata mismatch: %+v", input)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"binding": map[string]any{"generation": 1}, "recovery": map[string]any{"processResume": false}})
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "local-test-token")
	input := `{"action":"bind","target":"personal","authority":"coordinator-personal","projectID":"project","sessionID":"session","root":"/workspace/project","acceptedRevision":"sha256:` + strings.Repeat("a", 64) + `","dependencyPolicy":"npm-lockfile-v1"}`
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(input)}
	if err := app.Run(context.Background(), []string{"project-checkpoint", "--id", "cbx_project", "--request-stdin", "--json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"processResume":false`) {
		t.Fatal("recovery boundary omitted")
	}
}
