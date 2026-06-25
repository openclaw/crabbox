package flue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestRunBuildsArchiveRequestParsesNoisyResponseAndCleansUp(t *testing.T) {
	repo := newGitRepo(t)
	secret := "secret-token-value"
	var requestPath, archivePath string
	runner := &recordingRunner{fn: func(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
		input := decodeCLIInputArg(t, req.Args)
		requestPath = input.RequestFile
		if strings.Contains(strings.Join(req.Args, " "), secret) {
			t.Fatalf("secret leaked in flue argv: %v", req.Args)
		}
		data, err := os.ReadFile(requestPath)
		if err != nil {
			t.Fatalf("read request file during spawn: %v", err)
		}
		protocolReq, err := ParseRequest(data)
		if err != nil {
			t.Fatalf("parse request: %v", err)
		}
		archivePath = protocolReq.WorkspaceArchive
		if _, err := os.Stat(archivePath); err != nil {
			t.Fatalf("archive not present during spawn: %v", err)
		}
		if protocolReq.Workspace != "/workspace/test" || !reflect.DeepEqual(protocolReq.Command, []string{"echo", "ok"}) {
			t.Fatalf("protocol request=%#v", protocolReq)
		}
		if protocolReq.Env["SECRET_TOKEN"] != secret {
			t.Fatalf("request env=%#v", protocolReq.Env)
		}
		return LocalCommandResult{ExitCode: 0, Stdout: "progress line\n" + mustResponseJSON(t, Response{
			ProtocolVersion: protocolVersion,
			Operation:       operationRun,
			LeaseID:         protocolReq.LeaseID,
			Slug:            protocolReq.Slug,
			ExitCode:        0,
			Stdout:          "ok\n",
			Stderr:          "warn\n",
			Timing:          ResponseTiming{RunMs: 42, TotalMs: 50},
		}) + "\n"}, nil
	}}
	cfg := testConfig()
	cfg.Flue.Workdir = "/workspace/test"
	var stdout, stderr bytes.Buffer
	backend := testBackend(cfg, runner, &stdout, &stderr)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:       Repo{Name: "my-app", Root: repo},
		Command:    []string{"echo", "ok"},
		Env:        map[string]string{"SECRET_TOKEN": secret},
		EnvSummary: true,
		TimingJSON: true,
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if result.Provider != providerName || !result.SyncDelegated || result.ExitCode != 0 || result.Command.Milliseconds() != 42 {
		t.Fatalf("result=%#v", result)
	}
	if stdout.String() != "ok\n" || !strings.Contains(stderr.String(), "warn\n") || !strings.Contains(stderr.String(), `"syncDelegated":true`) {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	call := runner.onlyCall(t)
	wantPrefix := []string{"run", "workflow:crabbox-runner", "--target", "node", "--input"}
	if len(call.Args) < len(wantPrefix) || !reflect.DeepEqual(call.Args[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("args=%#v want prefix %#v", call.Args, wantPrefix)
	}
	if _, err := os.Stat(requestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("request file cleanup stat err=%v", err)
	}
	if _, err := os.Stat(archivePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive cleanup stat err=%v", err)
	}
}

func TestRunReturnsCommandExitCodeFromProtocolResponse(t *testing.T) {
	runner := &recordingRunner{fn: func(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
		input := decodeCLIInputArg(t, req.Args)
		protocolReq := readProtocolRequest(t, input.RequestFile)
		return LocalCommandResult{ExitCode: 0, Stdout: mustResponseJSON(t, Response{
			ProtocolVersion: protocolVersion,
			Operation:       operationRun,
			LeaseID:         protocolReq.LeaseID,
			Slug:            protocolReq.Slug,
			ExitCode:        7,
			Stdout:          "partial\n",
			Stderr:          "failed\n",
		})}, nil
	}}
	var stdout, stderr bytes.Buffer
	result, err := testBackend(testConfig(), runner, &stdout, &stderr).Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: newGitRepo(t)},
		Command: []string{"false"},
	})
	var exitErr core.ExitError
	if !core.AsExitError(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("Run err=%v result=%#v", err, result)
	}
	if result.ExitCode != 7 || result.Status != core.RunStatusFailed || result.ErrorKind != core.RunErrorCommandExit {
		t.Fatalf("result=%#v", result)
	}
	if stdout.String() != "partial\n" || !strings.Contains(stderr.String(), "failed\n") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunRejectsMalformedResponseWithoutLeakingEnv(t *testing.T) {
	secret := "very-secret-value"
	runner := &recordingRunner{fn: func(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{ExitCode: 0, Stdout: "human log\nnot-json\n"}, nil
	}}
	_, err := testBackend(testConfig(), runner, io.Discard, io.Discard).Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: newGitRepo(t)},
		Command: []string{"echo", "ok"},
		Env:     map[string]string{"TOKEN": secret},
	})
	if err == nil || !strings.Contains(err.Error(), "protocol JSON response") {
		t.Fatalf("Run err=%v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("malformed response error leaked env: %v", err)
	}
}

func TestRunRedactsWorkflowFailure(t *testing.T) {
	secret := "flue-secret-value"
	runner := &recordingRunner{fn: func(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{ExitCode: 9, Stderr: "workflow saw " + secret}, errors.New("exit status 9")
	}}
	_, err := testBackend(testConfig(), runner, io.Discard, io.Discard).Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: newGitRepo(t)},
		Command: []string{"echo", "ok"},
		Env:     map[string]string{"TOKEN": secret},
	})
	var exitErr core.ExitError
	if !core.AsExitError(err, &exitErr) || exitErr.Code != 9 {
		t.Fatalf("Run err=%v", err)
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("workflow failure was not redacted: %v", err)
	}
}

func TestRunClassifiesFlueTimeout(t *testing.T) {
	runner := &recordingRunner{fn: func(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{}, context.DeadlineExceeded
	}}
	result, err := testBackend(testConfig(), runner, io.Discard, io.Discard).Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: newGitRepo(t)},
		Command: []string{"sleep", "60"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run err=%v", err)
	}
	if result.Status != core.RunStatusTimedOut || result.ErrorKind != core.RunErrorTimeout {
		t.Fatalf("result=%#v", result)
	}
}

func TestRunRejectsUnsupportedOptionsBeforeSpawn(t *testing.T) {
	tests := []struct {
		name string
		cfg  func(Config) Config
		req  RunRequest
		want string
	}{
		{name: "cloudflare target", cfg: func(cfg Config) Config { cfg.Flue.Target = "cloudflare"; return cfg }, req: RunRequest{Command: []string{"true"}}, want: "target=node only"},
		{name: "no sync", req: RunRequest{NoSync: true, Command: []string{"true"}}, want: "--no-sync is not supported"},
		{name: "sync only", req: RunRequest{SyncOnly: true, Command: []string{"true"}}, want: "--sync-only is not supported"},
		{name: "lease id", req: RunRequest{ID: "cbx_123", Command: []string{"true"}}, want: "persistent lease ids"},
		{name: "keep", req: RunRequest{Keep: true, Command: []string{"true"}}, want: "persistent lease ids"},
		{name: "desktop", req: RunRequest{Options: core.LeaseOptions{Desktop: true}, Command: []string{"true"}}, want: "desktop"},
		{name: "tailscale", req: RunRequest{Options: core.LeaseOptions{Tailscale: core.TailscaleConfig{Enabled: true}}, Command: []string{"true"}}, want: "Tailscale"},
		{name: "script", req: RunRequest{ScriptRequested: true, Command: []string{"true"}}, want: "--script is not supported"},
		{name: "fresh pr", req: RunRequest{FreshPR: core.FreshPRSpec{Owner: "example-org", Repo: "my-app", Number: 1}, Command: []string{"true"}}, want: "--fresh-pr is not supported"},
		{name: "proof", req: RunRequest{EmitProof: "proof.json", Command: []string{"true"}}, want: "--emit-proof is not supported"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			if tc.cfg != nil {
				cfg = tc.cfg(cfg)
			}
			runner := &recordingRunner{}
			req := tc.req
			req.Repo = Repo{Name: "my-app", Root: newGitRepo(t)}
			_, err := testBackend(cfg, runner, io.Discard, io.Discard).Run(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run err=%v want %q", err, tc.want)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("runner called for rejected request: %#v", runner.calls)
			}
		})
	}
}

func TestLifecycleIsOneShot(t *testing.T) {
	backend := testBackend(testConfig(), &recordingRunner{}, io.Discard, io.Discard)
	if err := backend.Warmup(context.Background(), WarmupRequest{}); err == nil || !strings.Contains(err.Error(), "one-shot") {
		t.Fatalf("Warmup err=%v", err)
	}
	if leases, err := backend.List(context.Background(), ListRequest{}); err != nil || len(leases) != 0 {
		t.Fatalf("List leases=%#v err=%v", leases, err)
	}
	if _, err := backend.Status(context.Background(), StatusRequest{}); err == nil || !strings.Contains(err.Error(), "one-shot") {
		t.Fatalf("Status err=%v", err)
	}
	if err := backend.Stop(context.Background(), StopRequest{}); err == nil || !strings.Contains(err.Error(), "one-shot") {
		t.Fatalf("Stop err=%v", err)
	}
}

type recordingRunner struct {
	calls []LocalCommandRequest
	fn    func(context.Context, LocalCommandRequest) (LocalCommandResult, error)
}

func (r *recordingRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if r.fn != nil {
		return r.fn(ctx, req)
	}
	return LocalCommandResult{ExitCode: 0}, nil
}

func (r *recordingRunner) onlyCall(t *testing.T) LocalCommandRequest {
	t.Helper()
	if len(r.calls) != 1 {
		t.Fatalf("calls=%#v want one", r.calls)
	}
	return r.calls[0]
}

func testConfig() Config {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Flue.CLIPath = "flue"
	cfg.Flue.Workflow = defaultWorkflow
	cfg.Flue.Target = defaultTarget
	cfg.Flue.Workdir = defaultWorkdir
	cfg.Flue.TimeoutSecs = defaultTimeoutSecs
	return cfg
}

func testBackend(cfg Config, runner *recordingRunner, stdout, stderr io.Writer) *backend {
	rt := Runtime{Stdout: stdout, Stderr: stderr}
	if runner != nil {
		rt.Exec = runner
	}
	return &backend{spec: Provider{}.Spec(), cfg: cfg, rt: rt}
}

func decodeCLIInputArg(t *testing.T, args []string) CLIInput {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--input" {
			var raw map[string]json.RawMessage
			if err := json.Unmarshal([]byte(args[i+1]), &raw); err != nil {
				t.Fatalf("parse --input: %v", err)
			}
			if len(raw) != 1 {
				t.Fatalf("--input has keys=%v, want only requestFile", raw)
			}
			input, err := ParseCLIInput([]byte(args[i+1]))
			if err != nil {
				t.Fatalf("ParseCLIInput: %v", err)
			}
			return input
		}
	}
	t.Fatalf("args missing --input: %#v", args)
	return CLIInput{}
}

func readProtocolRequest(t *testing.T, path string) Request {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	req, err := ParseRequest(data)
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	return req
}

func mustResponseJSON(t *testing.T, resp Response) string {
	t.Helper()
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "alice@example.com")
	runGit(t, dir, "config", "user.name", "Alice")
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
