package dockersandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecIsDelegatedLinuxAndAliasFree(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName || spec.Family != "docker-sandbox" {
		t.Fatalf("spec identity = %#v", spec)
	}
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%q want delegated-run", spec.Kind)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%q want never", spec.Coordinator)
	}
	if !spec.Features.Has(core.FeatureRunSession) {
		t.Fatalf("features=%v want run-session", spec.Features)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v want linux only", spec.Targets)
	}
	if aliases := (Provider{}).Aliases(); len(aliases) != 0 {
		t.Fatalf("aliases=%v want none", aliases)
	}
}

func TestParseSandboxListToleratesArraysAndWrappers(t *testing.T) {
	for _, input := range []string{
		`[{"id":"abc","name":"crabbox-my-app-123abc","status":"running","agent":"shell","workspace":"/workspace"}]`,
		`{"sandboxes":[{"sandbox_id":"abc","sandbox_name":"crabbox-my-app-123abc","state":"ready","working_dir":"/workspace"}]}`,
		`{"items":[{"Name":"crabbox-my-app-123abc","Status":"Started"}]}`,
	} {
		records, err := parseSandboxList(input)
		if err != nil {
			t.Fatalf("parseSandboxList(%s): %v", input, err)
		}
		if len(records) != 1 {
			t.Fatalf("records=%#v want one", records)
		}
		if records[0].Name != "crabbox-my-app-123abc" {
			t.Fatalf("record name=%q", records[0].Name)
		}
	}
}

func TestRunCreatesExecsAndRemovesEphemeralSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := newRunner(map[string]scriptedReply{
		"create": {stdout: ""},
		"exec":   {stdout: "ok\n"},
		"rm":     {stdout: ""},
	}, nil)
	repoRoot := t.TempDir()
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(newTestConfig(), runner, &stdout, &stderr)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: repoRoot},
		Command: []string{"echo", "ok"},
	})
	if err != nil {
		t.Fatalf("Run err=%v stderr=%s", err, stderr.String())
	}
	if result.ExitCode != 0 || !result.SyncDelegated || result.Provider != providerName || result.LeaseID == "" || result.Slug == "" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if got, want := callVerbs(runner), []string{"create", "exec", "rm"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("verbs=%v want %v", got, want)
	}
	create := findCall(runner, "create")
	if create == nil {
		t.Fatal("missing create call")
	}
	for _, want := range []string{"create", "--name", "shell"} {
		if !containsArg(create.Args, want) {
			t.Fatalf("create args=%v missing %q", create.Args, want)
		}
	}
	if !containsArg(create.Args, t.TempDir()) {
		// The exact temp dir differs from the assertion temp dir; check any
		// absolute path reached the final workspace argument instead.
		if len(create.Args) == 0 || !strings.HasPrefix(create.Args[len(create.Args)-1], "/") {
			t.Fatalf("create args=%v missing workspace path", create.Args)
		}
	}
	execCall := findCall(runner, "exec")
	if execCall == nil {
		t.Fatal("missing exec call")
	}
	if !containsArg(execCall.Args, "--workdir") || !containsArg(execCall.Args, repoRoot) {
		t.Fatalf("exec args=%v missing workdir", execCall.Args)
	}
	if !containsArg(execCall.Args, "echo") || !containsArg(execCall.Args, "ok") {
		t.Fatalf("exec args=%v missing command", execCall.Args)
	}
	rm := findCall(runner, "rm")
	if rm == nil || !containsArg(rm.Args, "--force") {
		t.Fatalf("rm call=%#v missing --force", rm)
	}
	if claim, ok, err := resolveLeaseClaimForProvider(result.LeaseID, providerName); err != nil || ok || claim.LeaseID != "" {
		t.Fatalf("ephemeral claim still resolved claim=%#v ok=%t err=%v", claim, ok, err)
	}
}

func TestRunWithExistingIDReusesClaimedSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()
	leaseID := leasePrefix + "crabbox-my-app-abc123"
	if err := claimLeaseForRepoProviderPond(leaseID, "blue-box", providerName, "", repoRoot, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	runner := newRunner(map[string]scriptedReply{"exec": {stdout: "pwd\n"}}, nil)
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: repoRoot},
		ID:      "blue-box",
		Command: []string{"pwd"},
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if result.LeaseID != leaseID || result.Slug != "blue-box" {
		t.Fatalf("result=%#v", result)
	}
	if got, want := callVerbs(runner), []string{"exec"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("verbs=%v want %v", got, want)
	}
}

func TestListFiltersToCrabboxOwnedDockerSandboxes(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()
	owned := "crabbox-my-app-owned"
	if err := claimLeaseForRepoProviderPond(leasePrefix+owned, "owned", providerName, "", repoRoot, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepoProviderPond(leasePrefix+"crabbox-other-provider", "other", "tensorlake", "", repoRoot, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	runner := newRunner(map[string]scriptedReply{
		"ls": {stdout: `[
			{"name":"crabbox-my-app-owned","status":"running","agent":"shell"},
			{"name":"user-owned-sandbox","status":"running"},
			{"name":"crabbox-unclaimed","status":"running"},
			{"name":"crabbox-other-provider","status":"running"}
		]`},
	}, nil)
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	leases, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases=%#v want one owned lease", leases)
	}
	if leases[0].Name != owned || leases[0].Labels["slug"] != "owned" || leases[0].ServerType.Name != providerName {
		t.Fatalf("lease=%#v", leases[0])
	}
}

func TestStopRejectsUnclaimedIDBeforeCallingRM(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := newRunner(map[string]scriptedReply{"rm": {stdout: ""}}, nil)
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	err := backend.Stop(context.Background(), StopRequest{ID: "user-owned-sandbox"})
	if err == nil || !strings.Contains(err.Error(), "not claimed by Crabbox") {
		t.Fatalf("err=%v want unclaimed rejection", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("CLI invoked for unclaimed sandbox: %#v", runner.calls)
	}
}

func TestStopRemovesClaimedSandboxWithForce(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := leasePrefix + "crabbox-my-app-stopme"
	if err := claimLeaseForRepoProviderPond(leaseID, "stopme", providerName, "", t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	runner := newRunner(map[string]scriptedReply{"rm": {stdout: ""}}, nil)
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	if err := backend.Stop(context.Background(), StopRequest{ID: "stopme"}); err != nil {
		t.Fatalf("Stop err=%v", err)
	}
	rm := findCall(runner, "rm")
	if rm == nil || !reflect.DeepEqual(rm.Args, []string{"rm", "--force", "crabbox-my-app-stopme"}) {
		t.Fatalf("rm args=%v", rm.Args)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID, providerName); err != nil || ok {
		t.Fatalf("claim resolved after stop ok=%t err=%v", ok, err)
	}
}

func TestDoctorSuccessAndErrorGuidance(t *testing.T) {
	success := newRunner(map[string]scriptedReply{
		"version":  {stdout: "sbx version 0.1.0\n"},
		"ls":       {stdout: `[]`},
		"diagnose": {stdout: `{}`},
	}, nil)
	okResult, err := newTestBackend(newTestConfig(), success, io.Discard, io.Discard).Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor success err=%v", err)
	}
	if okResult.Status != "ok" || !strings.Contains(okResult.Message, "mutation=false") {
		t.Fatalf("doctor result=%#v", okResult)
	}

	missing := newRunner(map[string]scriptedReply{
		"version": {stderr: "not found", err: os.ErrNotExist},
	}, nil)
	_, err = newTestBackend(newTestConfig(), missing, io.Discard, io.Discard).Doctor(context.Background(), DoctorRequest{})
	if err == nil || !strings.Contains(err.Error(), "install the Docker Sandbox sbx CLI") {
		t.Fatalf("missing cli err=%v", err)
	}
	auth := newRunner(map[string]scriptedReply{
		"version": {stdout: "sbx version 0.1.0\n"},
		"ls":      {stderr: "not logged in", exitCode: 1},
	}, nil)
	_, err = newTestBackend(newTestConfig(), auth, io.Discard, io.Discard).Doctor(context.Background(), DoctorRequest{})
	if err == nil || !strings.Contains(err.Error(), "run sbx login") {
		t.Fatalf("auth err=%v", err)
	}
}

func TestUnsupportedAgentAndSSHOptionsRejectClearly(t *testing.T) {
	cfg := newTestConfig()
	cfg.DockerSandbox.Agent = "codex"
	if _, err := (Provider{}).Configure(cfg, Runtime{Exec: newRunner(nil, nil)}); err == nil || !strings.Contains(err.Error(), "v1 supports shell only") {
		t.Fatalf("Configure err=%v, want unsupported agent rejection", err)
	}
	err := rejectRunOptions(Provider{}.Spec(), RunRequest{Repo: Repo{Root: t.TempDir()}, Options: core.LeaseOptions{SSHUser: "root"}})
	if err == nil || !strings.Contains(err.Error(), "SSH-only") {
		t.Fatalf("rejectRunOptions err=%v, want SSH-only rejection", err)
	}
}

type recordingCommandRunner struct {
	mu       sync.Mutex
	calls    []core.LocalCommandRequest
	defaults map[string]scriptedReply
	scripts  map[string][]scriptedReply
}

type scriptedReply struct {
	stdout   string
	stderr   string
	exitCode int
	err      error
}

func (r *recordingCommandRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, req)
	key := scriptKey(req.Args)
	var reply scriptedReply
	if queue := r.scripts[key]; len(queue) > 0 {
		reply = queue[0]
		r.scripts[key] = queue[1:]
	} else if def, ok := r.defaults[key]; ok {
		reply = def
	}
	r.mu.Unlock()
	if req.Stdout != nil && reply.stdout != "" {
		_, _ = io.WriteString(req.Stdout, reply.stdout)
	}
	if req.Stderr != nil && reply.stderr != "" {
		_, _ = io.WriteString(req.Stderr, reply.stderr)
	}
	res := core.LocalCommandResult{ExitCode: reply.exitCode, Stdout: reply.stdout, Stderr: reply.stderr}
	return res, reply.err
}

func newRunner(defaults map[string]scriptedReply, sequenced map[string][]scriptedReply) *recordingCommandRunner {
	if defaults == nil {
		defaults = map[string]scriptedReply{}
	}
	if sequenced == nil {
		sequenced = map[string][]scriptedReply{}
	}
	return &recordingCommandRunner{defaults: defaults, scripts: sequenced}
}

func newTestConfig() Config {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.DockerSandbox.CLIPath = "sbx"
	cfg.DockerSandbox.Agent = "shell"
	return cfg
}

func newTestBackend(cfg Config, runner *recordingCommandRunner, stdout, stderr io.Writer) *backend {
	rt := Runtime{Stdout: stdout, Stderr: stderr, Exec: runner}
	return NewBackend(Provider{}.Spec(), cfg, rt).(*backend)
}

func scriptKey(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func callVerbs(r *recordingCommandRunner) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.calls))
	for _, call := range r.calls {
		out = append(out, scriptKey(call.Args))
	}
	return out
}

func findCall(r *recordingCommandRunner, verb string) *core.LocalCommandRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.calls {
		if scriptKey(r.calls[i].Args) == verb {
			return &r.calls[i]
		}
	}
	return nil
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestSBXErrorClassifiesVirtualization(t *testing.T) {
	err := sbxError([]string{"ls", "--json"}, 1, bytes.NewBufferString(""), bytes.NewBufferString("KVM unavailable"), nil)
	if err == nil || !strings.Contains(err.Error(), "virtualization") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunPropagatesCommandExit(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := newRunner(map[string]scriptedReply{
		"create": {stdout: ""},
		"exec":   {exitCode: 7, stderr: "failed\n"},
	}, nil)
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	_, err := backend.Run(context.Background(), RunRequest{Repo: Repo{Name: "my-app", Root: t.TempDir()}, Command: []string{"false"}, Keep: true})
	var exitErr core.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("err=%v want exit 7", err)
	}
}
