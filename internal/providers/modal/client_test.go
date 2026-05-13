package modal

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestModalExecPreservesRemoteExit125(t *testing.T) {
	runner := &modalClientRunner{writeResult: "125"}
	client := &modalPythonClient{cfg: newTestConfig(), rt: Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}

	code, err := client.Exec(context.Background(), modalExecRequest{
		SandboxID: "sb-123",
		Command:   []string{"bash", "-lc", "exit 125"},
		Stdout:    io.Discard,
		Stderr:    io.Discard,
	})
	if err != nil {
		t.Fatalf("Exec err=%v", err)
	}
	if code != 125 {
		t.Fatalf("exit=%d want 125", code)
	}
	if runner.resultPath == "" {
		t.Fatalf("missing result_path in payload %#v", runner.payload)
	}
}

func TestModalExecReportsTransportExit125(t *testing.T) {
	runner := &modalClientRunner{result: core.LocalCommandResult{ExitCode: modalTransportExitCode}}
	client := &modalPythonClient{cfg: newTestConfig(), rt: Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}

	code, err := client.Exec(context.Background(), modalExecRequest{
		SandboxID: "sb-123",
		Command:   []string{"bash", "-lc", "echo broken"},
		Stdout:    io.Discard,
		Stderr:    io.Discard,
	})
	if code != modalTransportExitCode {
		t.Fatalf("exit=%d want %d", code, modalTransportExitCode)
	}
	if err == nil || !strings.Contains(err.Error(), "transport failed") {
		t.Fatalf("err=%v want transport failure", err)
	}
}

type modalClientRunner struct {
	payload     map[string]any
	resultPath  string
	writeResult string
	result      core.LocalCommandResult
	err         error
}

func (r *modalClientRunner) Run(_ context.Context, req LocalCommandRequest) (core.LocalCommandResult, error) {
	if len(req.Args) >= 3 {
		if err := json.Unmarshal([]byte(req.Args[2]), &r.payload); err == nil {
			r.resultPath, _ = r.payload["result_path"].(string)
			if r.writeResult != "" && r.resultPath != "" {
				_ = os.WriteFile(r.resultPath, []byte(r.writeResult), 0o600)
			}
		}
	}
	return r.result, r.err
}
