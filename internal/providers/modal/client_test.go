package modal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
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

func TestModalCreateScriptScopesNamedSecretsThroughParentApp(t *testing.T) {
	python, err := osexec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "modal.py"), []byte(`
class App:
    @staticmethod
    def lookup(name, **kwargs):
        assert name == "crabbox-canary"
        assert kwargs == {"create_if_missing": True, "environment_name": "my-app-dev"}
        return "app-handle"

class Image:
    @staticmethod
    def from_registry(name):
        assert name == "python:3.13-slim"
        return "image-handle"

class Secret:
    @staticmethod
    def from_name(name, **kwargs):
        assert kwargs == {"environment_name": "my-app-dev"}
        return name

class CreatedSandbox:
    object_id = "sb-canary"
    name = ""
    def poll(self):
        return None
    def get_tags(self):
        return {}
    def set_tags(self, tags):
        pass
    def detach(self):
        pass

class Sandbox:
    @staticmethod
    def create(**kwargs):
        assert "environment_name" not in kwargs
        assert kwargs["app"] == "app-handle"
        assert kwargs["image"] == "image-handle"
        assert kwargs["secrets"] == ["example", "sample"]
        return CreatedSandbox()
`), 0o600); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(modalCreateSandboxRequest{
		App:            "crabbox-canary",
		Image:          "python:3.13-slim",
		TimeoutSeconds: 300,
		Environment:    "my-app-dev",
		Secrets:        []string{"example", "sample"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := osexec.Command(python, "-c", modalCreateScript, string(payload))
	cmd.Env = append(os.Environ(), "PYTHONPATH="+dir, "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("create script failed: %v; output=%s", err, out)
	}
	var sandbox modalSandbox
	if err := json.Unmarshal(out, &sandbox); err != nil {
		t.Fatalf("decode output %q: %v", out, err)
	}
	if sandbox.ID != "sb-canary" || sandbox.Status != "running" {
		t.Fatalf("sandbox=%#v", sandbox)
	}
}

func TestModalListCarriesConfiguredEnvironment(t *testing.T) {
	runner := &modalClientRunner{stdout: "[]\n"}
	cfg := newTestConfig()
	cfg.Modal.Environment = "my-app-dev"
	client := &modalPythonClient{cfg: cfg, rt: Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}

	if _, err := client.ListSandboxes(context.Background(), map[string]string{"crabbox": "true"}); err != nil {
		t.Fatal(err)
	}
	if runner.payload["environment"] != "my-app-dev" {
		t.Fatalf("list payload=%#v", runner.payload)
	}
}

func TestModalListScriptScopesAppLookupToEnvironment(t *testing.T) {
	python, err := osexec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "modal.py"), []byte(`
class AppHandle:
    app_id = "app-canary"

class App:
    @staticmethod
    def lookup(name, **kwargs):
        assert name == "crabbox-canary"
        assert kwargs == {"create_if_missing": True, "environment_name": "my-app-dev"}
        return AppHandle()

class Sandbox:
    @staticmethod
    def list(**kwargs):
        assert kwargs == {"app_id": "app-canary", "tags": {"crabbox": "true"}}
        return []
`), 0o600); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"app":         "crabbox-canary",
		"environment": "my-app-dev",
		"tags":        map[string]string{"crabbox": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := osexec.Command(python, "-c", modalListScript, string(payload))
	cmd.Env = append(os.Environ(), "PYTHONPATH="+dir, "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list script failed: %v; output=%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "[]" {
		t.Fatalf("list output=%q", out)
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

func TestModalExecScriptExitsTransportOnStreamCopyFailure(t *testing.T) {
	python, err := osexec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "modal.py"), []byte(`
class FailingStream:
    def __iter__(self):
        return self
    def __next__(self):
        raise RuntimeError("stdout copy boom")

class EmptyStream:
    def __iter__(self):
        return iter(())

class Proc:
    def __init__(self):
        self.stdout = FailingStream()
        self.stderr = EmptyStream()
    def wait(self):
        return 0

class Sandbox:
    @staticmethod
    def from_id(_sandbox_id):
        return Sandbox()
    def exec(self, *command, **kwargs):
        return Proc()
`), 0o600); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(dir, "result.txt")
	payload, err := json.Marshal(map[string]any{
		"sandbox_id":  "sb-123",
		"command":     []string{"true"},
		"result_path": resultPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := osexec.Command(python, "-c", modalExecScript, string(payload))
	cmd.Env = append(os.Environ(), "PYTHONPATH="+dir)
	out, err := cmd.CombinedOutput()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err=%v want exit error; output=%s", err, out)
	}
	if exitErr.ExitCode() != modalTransportExitCode {
		t.Fatalf("exit=%d want %d; output=%s", exitErr.ExitCode(), modalTransportExitCode, out)
	}
	if !strings.Contains(string(out), "stream copy failed") || !strings.Contains(string(out), "stdout copy boom") {
		t.Fatalf("output=%q want stream failure details", out)
	}
	if data, err := os.ReadFile(resultPath); err == nil && strings.TrimSpace(string(data)) == "0" {
		t.Fatalf("result file reports remote success after stream failure")
	}
}

type modalClientRunner struct {
	payload     map[string]any
	resultPath  string
	writeResult string
	stdout      string
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
	if r.stdout != "" && req.Stdout != nil {
		_, _ = io.WriteString(req.Stdout, r.stdout)
	}
	return r.result, r.err
}
