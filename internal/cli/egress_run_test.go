package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestEgressRunCommandFailureJoinsBridge(t *testing.T) {
	clearConfigEnv(t)
	isolateRunTestUserDirs(t, t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	closed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.CloseNow()
		_, _, _ = ws.Read(ctx)
		close(closed)
	}))
	defer server.Close()
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	bridge, err := app.connectEgressHost(ctx, &CoordinatorClient{BaseURL: server.URL}, "cbx_abcdef123456", "egress_0123456789abcdef0123456789abcdef", "egress_run", "", []string{"example.com"})
	if err != nil {
		t.Fatal(err)
	}
	err = app.runWithEgress(ctx, bridge, []string{"example.com"}, nil, []string{"--unsupported-run-option"})
	assertEgressExitCode(t, err, 2)
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("command failure left its bridge connected")
	}
}

func TestEgressRunHostCredentialIsExcludedFromRunEnvironment(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	t.Chdir(dir)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	t.Setenv("HOST_MODEL_PROXY", "http://operator:synthetic-password@127.0.0.1:3128")
	t.Setenv("HOST_MODEL_PUBLIC", "preserved")
	runModuleRuntimeTestRequests = nil
	t.Cleanup(func() { runModuleRuntimeTestRequests = nil })
	app := App{
		Stdout: io.Discard, Stderr: io.Discard,
		Stdin:          strings.NewReader("export default { fetch() { return new Response('ok') } }"),
		runEnvDenylist: []string{"HOST_MODEL_PROXY"},
	}
	if err := app.runCommand(t.Context(), []string{"--provider", "module-runtime-test", "--script-stdin", "--allow-env", "HOST_MODEL_*"}); err != nil {
		t.Fatal(err)
	}
	if len(runModuleRuntimeTestRequests) != 1 {
		t.Fatalf("workload requests = %d", len(runModuleRuntimeTestRequests))
	}
	env := runModuleRuntimeTestRequests[0].Env
	if _, found := env["HOST_MODEL_PROXY"]; found || env["HOST_MODEL_PUBLIC"] != "preserved" {
		t.Fatal("run environment did not isolate the host credential while preserving allowed public values")
	}
}
