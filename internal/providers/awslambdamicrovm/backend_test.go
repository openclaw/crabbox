package awslambdamicrovm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
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
	healthErr   error
	healthCheck func(context.Context, microVM) error
	exitCode    int
	execErr     error
	commands    []string
	uploads     []string
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func (f *fakeRunner) Health(ctx context.Context, vm microVM) error {
	if f.healthCheck != nil {
		return f.healthCheck(ctx, vm)
	}
	return f.healthErr
}

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

func TestWarmupWarnsWhenKeepIsFalse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	control := &fakeControlPlane{}
	runner := &fakeRunner{}
	b := testBackend(control, runner, io.Discard)
	var stderr bytes.Buffer
	b.rt.Stderr = &stderr
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: "/repo", Name: "my-app"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "warmup keeps the MicroVM until explicit stop") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestRunnerClientExecPreservesBinaryOutput(t *testing.T) {
	want := []byte{0xff, 0x00, 0xfe}
	dataEvent, err := json.Marshal(runnerEvent{Stream: "stdout", Data: want})
	if err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	exitEvent, err := json.Marshal(runnerEvent{ExitCode: &exitCode})
	if err != nil {
		t.Fatal(err)
	}
	body := append(append(dataEvent, '\n'), append(exitEvent, '\n')...)
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("X-aws-proxy-auth") != "token" {
			t.Fatalf("missing auth header: %v", req.Header)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    req,
		}, nil
	})}
	client := newRunnerClient(&fakeControlPlane{}, httpClient, "eu-west-1")
	var stdout bytes.Buffer
	gotExit, err := client.Exec(context.Background(), microVM{ID: "mvm-test", Endpoint: "mvm-test.lambda-microvm.eu-west-1.on.aws"}, "true", "/workspace/crabbox", nil, &stdout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if gotExit != 0 || !bytes.Equal(stdout.Bytes(), want) {
		t.Fatalf("exit=%d stdout=%x want=%x", gotExit, stdout.Bytes(), want)
	}
}

func TestWaitReadyBoundsRunnerHealthProbe(t *testing.T) {
	control := &fakeControlPlane{
		vm: microVM{ID: "mvm-test", Endpoint: "mvm-test.lambda-microvm.eu-west-1.on.aws", ImageARN: testImageARN, ImageVersion: "1", State: "RUNNING"},
	}
	runner := &fakeRunner{
		healthCheck: func(ctx context.Context, _ microVM) error {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("runner health context had no deadline")
			}
			if remaining := time.Until(deadline); remaining <= 0 || remaining > runnerHealthProbeTimeout+time.Second {
				t.Fatalf("runner health deadline remaining=%s", remaining)
			}
			return nil
		},
	}
	b := testBackend(control, runner, io.Discard)
	if _, err := b.waitReady(context.Background(), control, runner, control.vm); err != nil {
		t.Fatal(err)
	}
}

func TestProviderFlagsAndEndpointValidation(t *testing.T) {
	cfg := core.BaseConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--aws-lambda-microvm-region", "eu-west-1", "--aws-lambda-microvm-image", testImageARN, "--aws-lambda-microvm-workdir", "/work/project", "--aws-lambda-microvm-forget-missing"}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.AWSLambdaMicroVM.Workdir != "/work/project" || !cfg.AWSLambdaMicroVM.ForgetMissing {
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

func TestProviderRejectsBroadWorkdirs(t *testing.T) {
	for _, workdir := range []string{"/", "/tmp", "/work", "/workspace", "/home", "/root", "/usr", "/var", "/etc"} {
		cfg := core.BaseConfig()
		cfg.Provider = providerName
		cfg.AWSRegion = "eu-west-1"
		cfg.AWSLambdaMicroVM.Image = testImageARN
		cfg.AWSLambdaMicroVM.Workdir = workdir
		if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "too broad") {
			t.Fatalf("validateConfig(%q) err=%v, want too broad", workdir, err)
		}
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AWSRegion = "eu-west-1"
	cfg.AWSLambdaMicroVM.Image = testImageARN
	cfg.AWSLambdaMicroVM.Workdir = "/workspace/crabbox"
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("validateConfig dedicated workdir: %v", err)
	}
}

func TestProviderRejectsTailscaleOptions(t *testing.T) {
	base := func() core.Config {
		cfg := core.BaseConfig()
		cfg.Provider = providerName
		cfg.AWSRegion = "eu-west-1"
		cfg.AWSLambdaMicroVM.Image = testImageARN
		cfg.AWSLambdaMicroVM.Workdir = "/workspace/crabbox"
		return cfg
	}
	cfg := base()
	cfg.Tailscale.Enabled = true
	if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "does not support Tailscale") {
		t.Fatalf("tailscale enabled err=%v", err)
	}
	cfg = base()
	cfg.Network = core.NetworkTailscale
	if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "does not support Tailscale") {
		t.Fatalf("tailscale network err=%v", err)
	}
}

func TestProviderRejectsSubMinuteIdleTimeout(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AWSRegion = "eu-west-1"
	cfg.AWSLambdaMicroVM.Image = testImageARN
	cfg.AWSLambdaMicroVM.Workdir = "/workspace/crabbox"
	cfg.IdleTimeout = 59 * time.Second
	if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "at least 60s") {
		t.Fatalf("sub-minute idle timeout err=%v", err)
	}
	cfg.IdleTimeout = time.Minute
	if err := (Provider{}).ValidateConfig(cfg); err != nil {
		t.Fatalf("one-minute idle timeout err=%v", err)
	}
}

func TestLeaseOperationLockRejectsConcurrentOwner(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := leasePrefix + "locktest"
	unlock, err := lockAWSLambdaMicroVMLeaseOperation(context.Background(), leaseID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := lockAWSLambdaMicroVMLeaseOperation(ctx, leaseID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock err=%v, want context deadline", err)
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
