package awslambdamicrovm

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const testImageARN = "arn:aws:lambda:eu-west-1:123456789012:microvm-image:crabbox-runner"

type fakeControlPlane struct {
	vm         microVM
	calls      []string
	runRequest runMicroVMRequest
	runErr     error
}

func (f *fakeControlPlane) Run(_ context.Context, req runMicroVMRequest) (microVM, error) {
	f.calls = append(f.calls, "run")
	f.runRequest = req
	if f.runErr != nil {
		return microVM{}, f.runErr
	}
	f.vm = microVM{ID: "mvm-test", Endpoint: "mvm-test.lambda-microvm.eu-west-1.on.aws", ImageARN: req.ImageARN, ImageVersion: "1", State: "RUNNING", StartedAt: time.Now()}
	return f.vm, nil
}

func (f *fakeControlPlane) Get(_ context.Context, id string) (microVM, error) {
	f.calls = append(f.calls, "get:"+id)
	if f.vm.ID == "" || f.vm.State == "TERMINATED" {
		return microVM{}, errors.New("resource not found")
	}
	return f.vm, nil
}

func (f *fakeControlPlane) Probe(context.Context, string, string) error {
	f.calls = append(f.calls, "probe")
	return nil
}

func (f *fakeControlPlane) Terminate(_ context.Context, id string) error {
	f.calls = append(f.calls, "terminate:"+id)
	f.vm.State = "TERMINATED"
	return nil
}

func (f *fakeControlPlane) Suspend(_ context.Context, id string) error {
	f.calls = append(f.calls, "suspend:"+id)
	f.vm.State = "SUSPENDED"
	return nil
}

func (f *fakeControlPlane) Resume(_ context.Context, id string) error {
	f.calls = append(f.calls, "resume:"+id)
	f.vm.State = "RUNNING"
	return nil
}

func (f *fakeControlPlane) AuthToken(context.Context, string) (string, error) { return "token", nil }

type fakeRunner struct {
	healthErr error
	exitCode  int
	execErr   error
	commands  []string
	uploads   []string
}

func (f *fakeRunner) Health(context.Context, microVM) error { return f.healthErr }

func (f *fakeRunner) Upload(_ context.Context, _ microVM, path string, body io.Reader) error {
	_, _ = io.Copy(io.Discard, body)
	f.uploads = append(f.uploads, path)
	return nil
}

func (f *fakeRunner) Exec(_ context.Context, _ microVM, command, _ string, _ map[string]string, stdout, _ io.Writer) (int, error) {
	f.commands = append(f.commands, command)
	if strings.Contains(command, "runner-ok") {
		_, _ = io.WriteString(stdout, "runner-ok")
	}
	return f.exitCode, f.execErr
}

func TestRunSyncsExecutesAndTerminatesOneShot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := testRepo(t)
	control := &fakeControlPlane{}
	runner := &fakeRunner{}
	var stdout bytes.Buffer
	b := testBackend(control, runner, &stdout)
	result, err := b.Run(context.Background(), RunRequest{Repo: Repo{Root: repo, Name: "my-app"}, Command: []string{"printf", "runner-ok"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != providerName || result.ExitCode != 0 || !result.SyncDelegated || stdout.String() != "runner-ok" {
		t.Fatalf("result=%#v stdout=%q", result, stdout.String())
	}
	if len(runner.uploads) != 1 || !slices.ContainsFunc(runner.commands, func(command string) bool { return strings.Contains(command, "tar -xzf") }) {
		t.Fatalf("uploads=%v commands=%v", runner.uploads, runner.commands)
	}
	if !slices.Contains(control.calls, "terminate:mvm-test") || result.Session == nil || result.Session.Kept {
		t.Fatalf("calls=%v session=%#v", control.calls, result.Session)
	}
	if got := control.runRequest; got.ImageARN != testImageARN || got.MaximumSeconds != 28800 || len(got.IngressConnectors) != 1 || len(got.EgressConnectors) != 1 {
		t.Fatalf("run request=%#v", got)
	}
}

func TestRetainedLifecyclePauseResumeStop(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	control := &fakeControlPlane{}
	runner := &fakeRunner{}
	b := testBackend(control, runner, io.Discard)
	result, err := b.Run(context.Background(), RunRequest{Repo: Repo{Root: testRepo(t), Name: "my-app"}, Command: []string{"true"}, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil || !result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	if err := b.Pause(context.Background(), PauseRequest{ID: result.LeaseID}); err != nil {
		t.Fatal(err)
	}
	if err := b.Resume(context.Background(), ResumeRequest{ID: result.LeaseID}); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(context.Background(), StopRequest{ID: result.LeaseID}); err != nil {
		t.Fatal(err)
	}
	want := []string{"suspend:mvm-test", "resume:mvm-test", "terminate:mvm-test"}
	for _, call := range want {
		if !slices.Contains(control.calls, call) {
			t.Fatalf("missing %q in %v", call, control.calls)
		}
	}
	if _, ok, err := resolveLeaseClaim(result.LeaseID); err != nil || ok {
		t.Fatalf("claim remains ok=%t err=%v", ok, err)
	}
}

func TestImageRotationKeepsOldLeaseManageableButBlocksReuse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	control := &fakeControlPlane{}
	runner := &fakeRunner{}
	b := testBackend(control, runner, io.Discard)
	result, err := b.Run(context.Background(), RunRequest{Repo: Repo{Root: testRepo(t), Name: "my-app"}, Command: []string{"true"}, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	b.cfg.AWSLambdaMicroVM.Image = "arn:aws:lambda:eu-west-1:123456789012:microvm-image:new-runner"
	if _, err := b.Status(context.Background(), StatusRequest{ID: result.LeaseID}); err != nil {
		t.Fatalf("status after image rotation: %v", err)
	}
	servers, err := b.List(context.Background(), ListRequest{})
	if err != nil || len(servers) != 1 {
		t.Fatalf("list after image rotation: servers=%#v err=%v", servers, err)
	}
	if _, err := b.Run(context.Background(), RunRequest{Repo: Repo{Root: "/repo", Name: "my-app"}, ID: result.LeaseID, Command: []string{"true"}, NoSync: true}); err == nil || !strings.Contains(err.Error(), "image identity mismatch") {
		t.Fatalf("reuse after image rotation err=%v", err)
	}
	if err := b.Stop(context.Background(), StopRequest{ID: result.LeaseID}); err != nil {
		t.Fatalf("stop after image rotation: %v", err)
	}
}

func TestNoSyncWorkspaceFailureStopsBeforeCommand(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	control := &fakeControlPlane{}
	runner := &fakeRunner{exitCode: 9}
	b := testBackend(control, runner, io.Discard)
	result, err := b.Run(context.Background(), RunRequest{Repo: Repo{Root: testRepo(t), Name: "my-app"}, Command: []string{"must-not-run"}, NoSync: true})
	if err == nil || result.ExitCode != 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(runner.commands) != 1 || !strings.Contains(runner.commands[0], "mkdir -p") {
		t.Fatalf("commands=%v", runner.commands)
	}
	if !slices.Contains(control.calls, "terminate:mvm-test") {
		t.Fatalf("one-shot cleanup missing: %v", control.calls)
	}
}

func TestCreateRollsBackWhenRunnerIsUnhealthy(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	control := &fakeControlPlane{}
	runner := &fakeRunner{healthErr: errors.New("runner unavailable")}
	b := testBackend(control, runner, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Warmup(ctx, WarmupRequest{Repo: Repo{Root: "/repo", Name: "my-app"}, Keep: true}); err == nil {
		t.Fatal("unhealthy runner unexpectedly became ready")
	}
	if !slices.Contains(control.calls, "terminate:mvm-test") {
		t.Fatalf("rollback missing: %v", control.calls)
	}
}

func TestProviderFlagsAndEndpointValidation(t *testing.T) {
	cfg := core.BaseConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--aws-lambda-microvm-region", "eu-west-1", "--aws-lambda-microvm-image", testImageARN, "--aws-lambda-microvm-workdir", "/work", "--aws-lambda-microvm-forget-missing"}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.AWSLambdaMicroVM.Workdir != "/work" || !cfg.AWSLambdaMicroVM.ForgetMissing {
		t.Fatalf("config=%#v", cfg.AWSLambdaMicroVM)
	}
	if _, err := endpointURL("mvm-test.lambda-microvm.eu-west-1.on.aws", "eu-west-1"); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"http://mvm-test.lambda-microvm.eu-west-1.on.aws", "https://example.com", "https://mvm-test.lambda-microvm.us-east-1.on.aws"} {
		if _, err := endpointURL(endpoint, "eu-west-1"); err == nil {
			t.Fatalf("accepted endpoint %q", endpoint)
		}
	}
}

func testBackend(control *fakeControlPlane, runner *fakeRunner, stdout io.Writer) *backend {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AWSRegion = "eu-west-1"
	cfg.AWSLambdaMicroVM.Image = testImageARN
	cfg.AWSLambdaMicroVM.Workdir = "/workspace/crabbox"
	cfg.IdleTimeout = time.Hour
	cfg.TTL = 0
	return &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: stdout, Stderr: io.Discard},
		newControl: func(context.Context, Config) (controlPlane, error) {
			return control, nil
		},
		newRunner: func(controlPlane, Config, Runtime) runnerAPI { return runner },
	}
}

func testRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", dir, "init", "--quiet").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", dir, "add", ".").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "init").Run(); err != nil {
		t.Fatal(err)
	}
	return dir
}
