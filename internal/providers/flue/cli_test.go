package flue

import (
	"context"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCLIAdapterBuildsFlueRunArgs(t *testing.T) {
	runner := &recordingRunner{fn: func(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
		input := decodeCLIInputArg(t, req.Args)
		if input.RequestFile != "/tmp/request.json" {
			t.Fatalf("requestFile=%q", input.RequestFile)
		}
		return LocalCommandResult{ExitCode: 0, Stdout: mustResponseJSON(t, Response{ProtocolVersion: protocolVersion, Operation: operationRun, ExitCode: 0})}, nil
	}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Flue = FlueConfig{
		CLIPath:     "/opt/flue/bin/flue",
		Root:        "/tmp/flue-project",
		Workflow:    "workflow:runner",
		Target:      "node",
		Config:      "/tmp/flue.config.ts",
		EnvFile:     "/tmp/flue.env",
		Output:      "json",
		Workdir:     defaultWorkdir,
		TimeoutSecs: 10,
	}
	cli, err := newFlueCLI(cfg, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.run(context.Background(), "/tmp/request.json", nil); err != nil {
		t.Fatalf("run err=%v", err)
	}
	call := runner.onlyCall(t)
	if call.Name != "/opt/flue/bin/flue" || call.Dir != "/tmp/flue-project" {
		t.Fatalf("call name/dir=%q/%q", call.Name, call.Dir)
	}
	want := []string{
		"run", "workflow:runner", "--target", "node", "--input", `{"requestFile":"/tmp/request.json"}`,
		"--root", "/tmp/flue-project",
		"--config", "/tmp/flue.config.ts",
		"--env", "/tmp/flue.env",
		"--output", "json",
	}
	if !reflect.DeepEqual(call.Args, want) {
		t.Fatalf("args=%#v want %#v", call.Args, want)
	}
	rawOutputLimit := defaultStdoutLimitBytes + defaultStderrLimitBytes
	if call.MaxCapturedOutputBytes <= rawOutputLimit || call.MaxCapturedOutputBytes < 6*rawOutputLimit {
		t.Fatalf("MaxCapturedOutputBytes=%d too small for JSON protocol output limit %d", call.MaxCapturedOutputBytes, rawOutputLimit)
	}
}

func TestCLIAdapterNormalizesRelativeRoot(t *testing.T) {
	runner := &recordingRunner{fn: func(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{ExitCode: 0, Stdout: mustResponseJSON(t, Response{ProtocolVersion: protocolVersion, Operation: operationRun, ExitCode: 0})}, nil
	}}
	cfg := testConfig()
	cfg.Flue.Root = "flue-runner"
	cli, err := newFlueCLI(cfg, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.run(context.Background(), "/tmp/request.json", nil); err != nil {
		t.Fatalf("run err=%v", err)
	}
	call := runner.onlyCall(t)
	wantRoot, err := filepath.Abs("flue-runner")
	if err != nil {
		t.Fatal(err)
	}
	if call.Dir != wantRoot {
		t.Fatalf("Dir=%q want %q", call.Dir, wantRoot)
	}
	inputIndex := -1
	for i, arg := range call.Args {
		if arg == "--root" {
			inputIndex = i
			break
		}
	}
	if inputIndex < 0 || inputIndex+1 >= len(call.Args) || call.Args[inputIndex+1] != wantRoot {
		t.Fatalf("args=%#v want --root %q", call.Args, wantRoot)
	}
}

func TestCLIAdapterRejectsUnsupportedTargetBeforeSpawn(t *testing.T) {
	cfg := testConfig()
	cfg.Flue.Target = "cloudflare"
	runner := &recordingRunner{}
	_, err := newFlueCLI(cfg, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "target=node only") {
		t.Fatalf("newFlueCLI err=%v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner called: %#v", runner.calls)
	}
}
