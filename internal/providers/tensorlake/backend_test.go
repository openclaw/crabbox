package tensorlake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func osExec(name string, args ...string) *osexec.Cmd { return osexec.Command(name, args...) }

func TestProviderSpec(t *testing.T) {
	p := Provider{}
	if p.Name() != "tensorlake" {
		t.Fatalf("Name=%q want tensorlake", p.Name())
	}
	if len(p.Aliases()) == 0 {
		t.Fatalf("expected aliases, got none")
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%v want delegated run", spec.Kind)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%v want never", spec.Coordinator)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v want [{linux}]", spec.Targets)
	}
	if spec.Features.Has(core.FeatureURLBridge) {
		t.Fatalf("features=%#v should not advertise unsupported URL bridge", spec.Features)
	}
	if !spec.Features.Has(core.FeatureRunSession) {
		t.Fatalf("features=%#v should advertise run-session", spec.Features)
	}
}

func TestProviderForResolvesNameAndAliases(t *testing.T) {
	for _, name := range []string{"tensorlake", "tl", "tensorlake-sbx"} {
		got, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q) err=%v", name, err)
		}
		if got.Name() != "tensorlake" {
			t.Fatalf("ProviderFor(%q).Name()=%q want tensorlake", name, got.Name())
		}
	}
}

func TestTensorlakeWorkdirRejectsRelative(t *testing.T) {
	cfg := newTestConfig()
	cfg.Tensorlake.Workdir = "relative/path"
	if _, err := tensorlakeWorkdir(cfg); err == nil {
		t.Fatalf("expected rejection of relative workdir")
	}
}

func TestTensorlakeWorkdirRejectsBroadPaths(t *testing.T) {
	for _, workdir := range []string{"/", "/tmp", "/workspace", "/workspace/.."} {
		t.Run(workdir, func(t *testing.T) {
			cfg := newTestConfig()
			cfg.Tensorlake.Workdir = workdir
			if _, err := tensorlakeWorkdir(cfg); err == nil || !strings.Contains(err.Error(), "too broad") {
				t.Fatalf("err=%v, want too broad rejection", err)
			}
		})
	}
}

func TestTensorlakeWorkdirCleansDedicatedPath(t *testing.T) {
	cfg := newTestConfig()
	cfg.Tensorlake.Workdir = " /workspace/crabbox/../project "
	got, err := tensorlakeWorkdir(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/workspace/project" {
		t.Fatalf("workdir=%q want /workspace/project", got)
	}
}

func TestTensorlakeWorkdirDefault(t *testing.T) {
	cfg := newTestConfig()
	cfg.Tensorlake.Workdir = ""
	got, err := tensorlakeWorkdir(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/workspace/crabbox" {
		t.Fatalf("default=%q want /workspace/crabbox", got)
	}
}

func TestParseSandboxIDPicksAlphanumericLine(t *testing.T) {
	cases := map[string]string{
		"3pryjysezwsnlex226i5h":                                 "3pryjysezwsnlex226i5h",
		"  561sdfohklnysghdfbgrz  ":                             "561sdfohklnysghdfbgrz",
		"sandbox created\n3pryjysezwsnlex226i5h\nfollowup line": "3pryjysezwsnlex226i5h",
		"": "",
		"some warning that contains UPPERCASE and is not the id": "",
	}
	for input, want := range cases {
		if got := parseSandboxID(input); got != want {
			t.Errorf("parseSandboxID(%q)=%q want %q", input, got, want)
		}
	}
}

func TestParseSandboxIdentityExtractsNativeFields(t *testing.T) {
	out := "ID: 3pryjysezwsnlex226i5h\nName: crabbox-app-aaa111\nNamespace: sandbox_ns\nStatus: running\nImage: ubuntu-minimal\n"
	got, err := parseSandboxIdentity(out)
	if err != nil || got.ID != "3pryjysezwsnlex226i5h" || got.Namespace != "sandbox_ns" || got.State != "running" {
		t.Fatalf("identity=%+v err=%v", got, err)
	}
	if _, err := parseSandboxIdentity(""); err == nil {
		t.Fatal("empty identity accepted")
	}
}

func TestIsReadyState(t *testing.T) {
	cases := map[string]bool{
		"running":    true,
		"  Running ": true,
		"ready":      true,
		"starting":   false,
		"terminated": false,
		"":           false,
	}
	for state, want := range cases {
		if got := isReadyState(state); got != want {
			t.Errorf("isReadyState(%q)=%v want %v", state, got, want)
		}
	}
}

func TestResolveLeaseIDRejectsUnclaimed(t *testing.T) {
	_, _, _, err := resolveTestLease("not-a-known-slug", "", false, 0)
	if err == nil || !strings.Contains(err.Error(), "not exactly claimed by Crabbox") {
		t.Fatalf("err=%v, want rejection of unclaimed sandbox", err)
	}
}

func TestResolveLeaseIDRejectsLeasePrefixWithoutClaim(t *testing.T) {
	_, _, _, err := resolveTestLease("tlsbx_unknown123", "", false, 0)
	if err == nil || !strings.Contains(err.Error(), "not exactly claimed by Crabbox") {
		t.Fatalf("err=%v, want rejection without local claim", err)
	}
}

func TestResolveLeaseIDUsesTensorlakeClaimWhenSlugCollides(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := core.ClaimLeaseForRepoProvider("tbx_abc123", "Blue Lobster", "blacksmith-testbox", "/repo-a", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := claimBoundTensorlakeForTest("tlsbx_tensorlake12345600000", "Blue Lobster", "/repo-b", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	leaseID, sandboxID, slug, err := resolveTestLease("blue-lobster", "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "tlsbx_tensorlake12345600000" || sandboxID != "tensorlake12345600000" {
		t.Fatalf("lease=%q sandbox=%q", leaseID, sandboxID)
	}
	if slug != "Blue Lobster" {
		t.Fatalf("slug=%q", slug)
	}
}

func TestResolveLeaseIDRejectsLegacySluglessClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "tlsbx_tensorlake12345600000"
	if err := core.ClaimLeaseForRepoProvider(leaseID, "", providerName, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := resolveTestLease(leaseID, "", false, 0); err == nil {
		t.Fatal("legacy claim was adopted")
	}
}

func TestResolveLeaseIDRequiresIdentifier(t *testing.T) {
	if _, _, _, err := resolveTestLease("", "", false, 0); err == nil {
		t.Fatalf("expected error for empty id")
	}
}

func TestStatusReturnsDescribeErrorWithoutWait(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "tlsbx_statusmissing12300000"
	if err := claimBoundTensorlakeForTest(leaseID, "status-missing", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	defer core.RemoveLeaseClaim(leaseID)
	runner := newRunner(map[string]scriptedReply{
		"sbx describe": {stderr: "sandbox not found\n", exitCode: 1},
	}, nil)
	backend := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), newTestRuntime(runner)).(*tensorlakeBackend)
	_, err := backend.Status(context.Background(), StatusRequest{ID: "status-missing"})
	if err == nil || !strings.Contains(err.Error(), "ownership control command failed") {
		t.Fatalf("Status err=%v, want describe failure", err)
	}
}

func TestStatusWaitTimeoutIncludesDescribeError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "tlsbx_statuswait12300000000"
	if err := claimBoundTensorlakeForTest(leaseID, "status-wait", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	defer core.RemoveLeaseClaim(leaseID)
	runner := newRunner(map[string]scriptedReply{
		"sbx describe": {stderr: "auth denied\n", exitCode: 1},
	}, nil)
	rt := newTestRuntime(runner)
	rt.Clock = &stepClock{now: time.Unix(0, 0), step: time.Second}
	backend := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), rt).(*tensorlakeBackend)
	_, err := backend.Status(context.Background(), StatusRequest{ID: "status-wait", Wait: true, WaitTimeout: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out waiting") || !strings.Contains(err.Error(), "ownership control command failed") {
		t.Fatalf("Status err=%v, want timeout with describe failure", err)
	}
}

func TestStatusWaitContextExpiryIncludesDescribeError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "tlsbx_statusctx123000000000"
	if err := claimBoundTensorlakeForTest(leaseID, "status-context", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	defer core.RemoveLeaseClaim(leaseID)
	runner := newRunner(map[string]scriptedReply{
		"sbx describe": {stderr: "auth denied\n", exitCode: 1},
	}, nil)
	backend := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), newTestRuntime(runner)).(*tensorlakeBackend)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := backend.Status(ctx, StatusRequest{ID: "status-context", Wait: true, WaitTimeout: time.Minute})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Status err=%v, want context cancellation with describe failure", err)
	}
}

type stepClock struct {
	now  time.Time
	step time.Duration
}

func (c *stepClock) Now() time.Time {
	c.now = c.now.Add(c.step)
	return c.now
}

func TestNewSandboxNameUsesRepoName(t *testing.T) {
	repo := Repo{Name: "carbbox"}
	name := newSandboxName(repo)
	if !strings.HasPrefix(name, "crabbox-carbbox-") {
		t.Fatalf("name=%q does not start with crabbox-carbbox-", name)
	}
}

func TestNewSandboxNameStripsRedundantPrefix(t *testing.T) {
	repo := Repo{Name: "crabbox-app"}
	name := newSandboxName(repo)
	if strings.HasPrefix(name, "crabbox-crabbox-") {
		t.Fatalf("name=%q double-prefixed", name)
	}
	if !strings.HasPrefix(name, "crabbox-app-") {
		t.Fatalf("name=%q does not start with crabbox-app-", name)
	}
}

func TestNewSandboxNameFitsTensorlakeLimit(t *testing.T) {
	repo := Repo{Name: strings.Repeat("very-long-repo-name-", 8)}
	name := newSandboxName(repo)
	if len(name) > 63 {
		t.Fatalf("name len=%d want <=63: %q", len(name), name)
	}
	if strings.HasSuffix(name, "-") || !strings.HasPrefix(name, "crabbox-") {
		t.Fatalf("invalid sandbox name: %q", name)
	}
}

// recordingCommandRunner is a fake CommandRunner that records every call and
// replies from a per-verb queue of scripted (stdout, stderr, exit, err)
// tuples. Replies are popped in order; if the queue for a verb is empty, the
// last reply (or zero value) is reused.
type recordingCommandRunner struct {
	resources   map[string]sandboxIdentity
	hook        func(core.LocalCommandRequest) (core.LocalCommandResult, error, bool)
	contextHook func(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error, bool)
	mu          sync.Mutex
	calls       []core.LocalCommandRequest
	scripts     map[string][]scriptedReply
	defaults    map[string]scriptedReply
}

type scriptedReply struct {
	stdout   string
	stderr   string
	exitCode int
	err      error
}

func (r *recordingCommandRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, req)
	r.mu.Unlock()
	if r.contextHook != nil {
		if result, err, handled := r.contextHook(ctx, req); handled {
			return result, err
		}
	}
	if r.hook != nil {
		if result, err, handled := r.hook(req); handled {
			return result, err
		}
	}
	r.mu.Lock()
	key := scriptKey(req.Args)
	var reply scriptedReply
	if queue := r.scripts[key]; len(queue) > 0 {
		reply = queue[0]
		r.scripts[key] = queue[1:]
	} else if def, ok := r.defaults[key]; ok {
		reply = def
	} else if key == "whoami" {
		reply = fixtureScopeReply(req)
	} else if key == "sbx describe" {
		id := req.Args[len(req.Args)-1]
		if item, ok := r.resources[id]; ok {
			reply.stdout = fmt.Sprintf("ID: %s\nName: %s\nNamespace: %s\nStatus: %s\n", item.ID, item.Name, item.Namespace, item.State)
		} else {
			reply.exitCode = 4
			reply.stderr = "fixture resource not found"
		}
	}
	if reply.err == nil && reply.exitCode == 0 {
		if key == "sbx create" {
			id := strings.TrimSpace(reply.stdout)
			if isLikelySandboxID(id) {
				r.resources[id] = sandboxIdentity{ID: id, Name: req.Args[len(req.Args)-1], Namespace: "sandbox_ns", State: "running"}
			}
		} else if key == "sbx terminate" {
			id := req.Args[len(req.Args)-1]
			if item, ok := r.resources[id]; ok {
				item.State = "terminated"
				r.resources[id] = item
			}
		}
	}
	r.mu.Unlock()
	if req.Stdout != nil && reply.stdout != "" {
		_, _ = io.WriteString(req.Stdout, reply.stdout)
	}
	if req.Stderr != nil && reply.stderr != "" {
		_, _ = io.WriteString(req.Stderr, reply.stderr)
	}
	res := core.LocalCommandResult{
		ExitCode: reply.exitCode,
		Stdout:   reply.stdout,
		Stderr:   reply.stderr,
	}
	return res, reply.err
}

func newRunner(defaults map[string]scriptedReply, sequenced map[string][]scriptedReply) *recordingCommandRunner {
	return &recordingCommandRunner{defaults: defaults, scripts: sequenced, resources: map[string]sandboxIdentity{}}
}

// scriptKey extracts the `sbx <verb>` portion of an argv slice, ignoring
// global flags so test scripts can match by subcommand alone.
func scriptKey(args []string) string {
	for i, a := range args {
		if a == "whoami" {
			return "whoami"
		}
		if a == "sbx" && i+1 < len(args) {
			return "sbx " + args[i+1]
		}
	}
	return ""
}

func newTestRuntime(runner *recordingCommandRunner) Runtime {
	return Runtime{
		Stdout: io.Discard,
		Stderr: io.Discard,
		Exec:   runner,
	}
}

func newTestConfig() Config {
	cfg := Config{}
	cfg.Tensorlake.APIKey = "tl_apiKey_test"
	cfg.Tensorlake.APIURL = "https://api.tensorlake.ai"
	cfg.Tensorlake.CLIPath = "tensorlake"
	cfg.Tensorlake.CPUs = 1.0
	cfg.Tensorlake.MemoryMB = 1024
	cfg.Tensorlake.DiskMB = 10240
	return cfg
}

func TestRunCreatesExecsAndTerminatesEphemeralSandbox(t *testing.T) {
	testutil.IsolateUserDirs(t)
	runner := newRunner(map[string]scriptedReply{
		"sbx create":    {stdout: "3pryjysezwsnlex226i5h\n"},
		"sbx exec":      {stdout: "hello\n"},
		"sbx terminate": {stdout: "3pryjysezwsnlex226i5h\n"},
	}, nil)
	cfg := newTestConfig()
	rt := newTestRuntime(runner)
	backend := NewTensorlakeBackend(Provider{}.Spec(), cfg, rt).(*tensorlakeBackend)
	repoRoot := t.TempDir()
	req := RunRequest{
		Repo:    Repo{Name: "carbbox", Root: repoRoot},
		Command: []string{"echo", "hello"},
		NoSync:  true,
	}
	defer func() {
		// Best-effort cleanup of the lease claim store side effects.
		_ = req
	}()
	result, err := backend.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d want 0", result.ExitCode)
	}
	if result.Status != core.RunStatusSucceeded || result.ErrorKind != core.RunErrorNone {
		t.Fatalf("status/error=%q/%q", result.Status, result.ErrorKind)
	}
	if result.Session == nil {
		t.Fatal("session=nil")
	}
	if result.Session.Provider != providerName || result.Session.LeaseID != "tlsbx_3pryjysezwsnlex226i5h" || result.Session.Slug == "" || result.Session.Reused || result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	if result.Session.CleanupCommand != "crabbox stop --provider tensorlake --id 'tlsbx_3pryjysezwsnlex226i5h'" {
		t.Fatalf("cleanup command=%q", result.Session.CleanupCommand)
	}
	verbs := callMutationVerbs(runner)
	// With --no-sync we still prepare the workdir (mkdir) before the user's command.
	want := []string{"sbx create", "sbx exec", "sbx exec", "sbx terminate"}
	if !reflect.DeepEqual(verbs, want) {
		t.Fatalf("verbs=%v want %v", verbs, want)
	}
	// `sbx exec` must target the captured sandbox ID, not the human name.
	// The user-command exec is the second exec call (the first is the mkdir prepare).
	execCall := findCallN(runner, "sbx exec", 1)
	if execCall == nil {
		t.Fatalf("missing user-command sbx exec call")
	}
	if !containsArg(execCall.Args, "3pryjysezwsnlex226i5h") {
		t.Fatalf("exec args=%v missing sandbox id", execCall.Args)
	}
	if !containsArg(execCall.Args, "echo") || !containsArg(execCall.Args, "hello") {
		t.Fatalf("exec args=%v missing user command", execCall.Args)
	}
	if !containsArg(execCall.Args, "-w") || !containsArg(execCall.Args, "/workspace/crabbox") {
		t.Fatalf("exec args=%v missing -w workdir", execCall.Args)
	}
	// API key must flow via env, never argv.
	if containsArgPrefix(execCall.Args, "tl_apiKey_") {
		t.Fatalf("API key leaked into argv: %v", execCall.Args)
	}
	if !containsEnv(execCall.Env, "TENSORLAKE_API_KEY=tl_apiKey_test") {
		t.Fatal("env missing the expected fixture API-key entry")
	}
}

func TestRunForwardsEnvViaUploadedProfile(t *testing.T) {
	testutil.IsolateUserDirs(t)
	runner := newRunner(map[string]scriptedReply{
		"sbx create":    {stdout: "envid0123456789012000\n"},
		"sbx exec":      {stdout: "ok\n"},
		"sbx cp":        {stdout: ""},
		"sbx terminate": {stdout: "envid0123456789012000\n"},
	}, nil)
	var stderr bytes.Buffer
	rt := newTestRuntime(runner)
	rt.Stderr = &stderr
	backend := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), rt).(*tensorlakeBackend)
	req := RunRequest{
		Repo:       Repo{Name: "carbbox", Root: t.TempDir()},
		Command:    []string{"printenv", "SECRET_TOKEN"},
		NoSync:     true,
		Env:        map[string]string{"SECRET_TOKEN": "super-secret"},
		EnvSummary: true,
		Options:    core.LeaseOptions{EnvAllow: []string{"SECRET_TOKEN"}},
	}
	if _, err := backend.Run(context.Background(), req); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	verbs := callMutationVerbs(runner)
	want := []string{"sbx create", "sbx exec", "sbx cp", "sbx exec", "sbx exec", "sbx terminate"}
	if !reflect.DeepEqual(verbs, want) {
		t.Fatalf("verbs=%v want %v", verbs, want)
	}
	if strings.Contains(stderr.String(), "super-secret") {
		t.Fatalf("secret leaked in stderr: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "SECRET_TOKEN=set len=12 secret=true") {
		t.Fatalf("missing redacted env summary: %s", stderr.String())
	}
	cp := findCall(runner, "sbx cp")
	if cp == nil || containsArgSubstring(cp.Args, "super-secret") {
		t.Fatalf("env upload leaked secret in argv: %#v", cp)
	}
	userExec := findCallN(runner, "sbx exec", 1)
	if userExec == nil {
		t.Fatalf("missing user exec")
	}
	if containsArgSubstring(userExec.Args, "super-secret") {
		t.Fatalf("secret leaked in exec argv: %v", userExec.Args)
	}
	if !containsArg(userExec.Args, "bash") || !containsArg(userExec.Args, "-lc") || !containsArgSubstring(userExec.Args, "/tmp/crabbox-env-") {
		t.Fatalf("exec args=%v missing env profile wrapper", userExec.Args)
	}
}

func TestRunCommandIntentSurvivesNativeCLIArgv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native POSIX CLI transport")
	}
	for _, withEnv := range []bool{false, true} {
		for _, scenario := range []string{"literal pipe", "literal assignment", "mixed operators", "inferred source", "plain argv", "explicit exit", "explicit empty"} {
			t.Run(fmt.Sprintf("env=%v/%s", withEnv, scenario), func(t *testing.T) {
				b, _, runner, claim := ownedTensorlakeFixture(t)
				root := t.TempDir()
				b.cfg.Tensorlake.Workdir = root
				t.Setenv("PATH", root+":/usr/bin:/bin")
				if err := os.WriteFile(filepath.Join(root, "FOO=x"), []byte("#!/bin/sh\nprintf 'literal:%s' \"$*\"\nexit 42\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				marker := filepath.Join(root, "must-not-exist")
				request := RunRequest{ID: claim.LeaseID, Repo: Repo{Root: claim.RepoRoot}, NoSync: true, Command: []string{"printf", "%s", "|", "touch", marker}, CommandLiteralArgs: map[int]bool{2: true}}
				if withEnv {
					request.Env = map[string]string{"FIXTURE": "quoted ' synthetic\n$literal", "PATH": root + ":/usr/bin:/bin"}
				}
				want, wantCode := "|touch"+marker, 0
				switch scenario {
				case "literal assignment":
					request.Command = []string{"FOO=x", "argument"}
					request.CommandLiteralArgs = map[int]bool{0: true}
					want = "literal:argument"
					wantCode = 42
				case "mixed operators":
					request.Command = []string{"printf", "%s", ";", "&&", "printf", "%s", "tail"}
					want = ";tail"
				case "inferred source":
					request.Command = []string{"printf '%s' source"}
					request.CommandLiteralArgs = nil
					want = "source"
				case "plain argv":
					request.Command = []string{"printf", "%s", "plain"}
					request.CommandLiteralArgs = nil
					want = "plain"
				case "explicit exit":
					request.Command = []string{"printf explicit; exit 7"}
					request.CommandLiteralArgs = nil
					request.ShellMode = true
					want, wantCode = "explicit", 7
				case "explicit empty":
					request.Command = []string{""}
					request.CommandLiteralArgs = nil
					request.ShellMode = true
					want = ""
				}
				var stdout bytes.Buffer
				b.rt.Stdout = &stdout
				var localPath, remotePath string
				t.Cleanup(func() {
					if remotePath != "" {
						_ = os.Remove(remotePath)
					}
				})
				runner.contextHook = func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
					switch scriptKey(req.Args) {
					case "sbx cp":
						localPath = req.Args[len(req.Args)-2]
						_, remotePath, _ = strings.Cut(req.Args[len(req.Args)-1], ":")
						data, err := os.ReadFile(localPath)
						if err != nil {
							return core.LocalCommandResult{}, err, true
						}
						file, err := os.OpenFile(remotePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
						if err != nil {
							return core.LocalCommandResult{}, err, true
						}
						_, writeErr := file.Write(data)
						return core.LocalCommandResult{}, errors.Join(writeErr, file.Close()), true
					case "sbx exec":
						idIndex := -1
						for i, arg := range req.Args {
							if arg == claim.CloudID {
								idIndex = i
								break
							}
						}
						if idIndex < 0 || idIndex+1 >= len(req.Args) {
							t.Fatalf("missing native target: %v", req.Args)
						}
						argv := req.Args[idIndex+1:]
						cmd := osexec.CommandContext(ctx, argv[0], argv[1:]...)
						cmd.Dir = root
						cmd.Env = []string{"PATH=" + root + ":/usr/bin:/bin", "HOME=" + root, "ENV=" + os.DevNull}
						out, err := cmd.CombinedOutput()
						if req.Stdout != nil {
							_, _ = req.Stdout.Write(out)
						}
						code := 0
						if err != nil {
							var exitErr *osexec.ExitError
							if !errors.As(err, &exitErr) {
								t.Fatal(err)
							}
							code = exitErr.ExitCode()
						}
						return core.LocalCommandResult{ExitCode: code}, err, true
					}
					return core.LocalCommandResult{}, nil, false
				}
				result, err := b.Run(t.Context(), request)
				if result.ExitCode != wantCode || stdout.String() != want || (err != nil) != (wantCode != 0) {
					t.Fatalf("output=%q exit=%d err=%v want=%q/%d", stdout.String(), result.ExitCode, err, want, wantCode)
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("literal sentinel created: %v", err)
				}
				if withEnv {
					for _, file := range []string{localPath, remotePath} {
						if file == "" {
							t.Fatal("profile upload missing")
						}
						if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("profile residue: %v", err)
						}
					}
				}
			})
		}
	}
}

func TestRunCleansPartialEnvUploadOnReusedSandbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native fixture requires POSIX /tmp and sh")
	}
	if _, err := osexec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%v", canceled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			b, _, runner, claim := ownedTensorlakeFixture(t)
			localPath, remotePath := "", ""
			t.Cleanup(func() {
				if remotePath != "" {
					_ = os.Remove(remotePath)
				}
			})
			uploadErr := errors.New("synthetic partial profile upload")
			runner.contextHook = func(callCtx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				if scriptKey(req.Args) == "sbx cp" {
					localPath = req.Args[len(req.Args)-2]
					_, remotePath, _ = strings.Cut(req.Args[len(req.Args)-1], ":")
					info, err := os.Stat(localPath)
					if err != nil {
						t.Fatal(err)
					}
					if info.Mode().Perm() != 0o600 {
						t.Fatalf("local profile permissions=%v", info.Mode().Perm())
					}
					data, err := os.ReadFile(localPath)
					if err != nil {
						t.Fatal(err)
					}
					f, err := os.OpenFile(remotePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
					if err != nil {
						t.Fatal(err)
					}
					_, writeErr := f.Write(data[:len(data)/2])
					closeErr := f.Close()
					if writeErr != nil || closeErr != nil {
						t.Fatalf("fixture write=%v close=%v", writeErr, closeErr)
					}
					if canceled {
						cancel()
						return core.LocalCommandResult{}, context.Canceled, true
					}
					return core.LocalCommandResult{ExitCode: 7}, uploadErr, true
				}
				if scriptKey(req.Args) == "sbx exec" && remotePath != "" && strings.Contains(req.Args[len(req.Args)-1], "rm -f "+shellQuote(remotePath)) {
					if _, ok := callCtx.Deadline(); !ok || callCtx.Err() != nil {
						t.Fatal("cleanup context must remain live and bounded")
					}
					err := osexec.CommandContext(callCtx, "sh", "-c", req.Args[len(req.Args)-1]).Run()
					return core.LocalCommandResult{}, err, true
				}
				return core.LocalCommandResult{}, nil, false
			}
			_, err := b.Run(ctx, RunRequest{ID: claim.LeaseID, Repo: Repo{Root: claim.RepoRoot}, NoSync: true, Command: []string{"user-workload"}, Env: map[string]string{"FIXTURE_VALUE": "synthetic-marker"}})
			if err == nil {
				t.Fatal("expected partial upload failure")
			}
			for _, path := range []string{localPath, remotePath} {
				if path == "" {
					t.Fatal("upload fixture did not run")
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("profile residue after failure: %s: %v", path, err)
				}
			}
			if findCall(runner, "sbx terminate") != nil {
				t.Fatal("reused sandbox terminated")
			}
			for _, call := range runner.calls {
				if containsArgSubstring(call.Args, "user-workload") {
					t.Fatal("user command ran after profile upload failure")
				}
			}
		})
	}
}

func TestEnvProfileCanceledBeforeAuthorizationDoesNotAcquireRemoteCustody(t *testing.T) {
	b, cli, runner, claim := ownedTensorlakeFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, cleanup, err := b.uploadEnvProfile(ctx, cli, claim, map[string]string{"FIXTURE": "value"})
	if err == nil || cleanup == nil {
		t.Fatalf("err=%v cleanup missing=%v", err, cleanup == nil)
	}
	cleanup(t.Context())
	if len(runner.calls) != 0 {
		t.Fatalf("native operation before authorized upload: %v", callMutationVerbs(runner))
	}
}

func TestEnvProfileCleanupRejectsChangedAuthority(t *testing.T) {
	for _, change := range []string{"claim", "namespace"} {
		t.Run(change, func(t *testing.T) {
			b, _, runner, claim := ownedTensorlakeFixture(t)
			var stderr bytes.Buffer
			b.rt.Stderr = &stderr
			localPath, remotePath := "", ""
			remoteFile := filepath.Join(t.TempDir(), "remote-profile")
			var successor core.LeaseClaim
			runner.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				if scriptKey(req.Args) == "sbx cp" {
					localPath = req.Args[len(req.Args)-2]
					_, remotePath, _ = strings.Cut(req.Args[len(req.Args)-1], ":")
					data, err := os.ReadFile(localPath)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(remoteFile, data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if scriptKey(req.Args) == "sbx exec" && containsArgSubstring(req.Args, "user-workload") {
					if change == "claim" {
						current, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID)
						if err != nil || !exists {
							t.Fatal("missing current claim", err)
						}
						successor = current
						successor.RepoRoot = t.TempDir()
						successor, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(current.LeaseID, current, successor)
						if err != nil {
							t.Fatal(err)
						}
					} else {
						item := runner.resources[claim.CloudID]
						item.Namespace = "successor_namespace"
						runner.resources[claim.CloudID] = item
					}
				}
				if scriptKey(req.Args) == "sbx exec" && remotePath != "" && strings.Contains(req.Args[len(req.Args)-1], "rm -f "+shellQuote(remotePath)) {
					t.Fatal("stale authority issued remote profile removal")
				}
				return core.LocalCommandResult{}, nil, false
			}
			_, err := b.Run(t.Context(), RunRequest{ID: claim.LeaseID, Repo: Repo{Root: claim.RepoRoot}, NoSync: true, Command: []string{"user-workload"}, Env: map[string]string{"FIXTURE": "synthetic"}})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stderr.String(), "env profile cleanup failed") {
				t.Fatal("missing cleanup warning")
			}
			if _, err := os.Stat(localPath); !os.IsNotExist(err) {
				t.Fatalf("local profile retained: %v", err)
			}
			if _, err := os.Stat(remoteFile); err != nil {
				t.Fatalf("remote fixture unexpectedly removed: %v", err)
			}
			if change == "claim" {
				assertTensorlakeClaimUnchanged(t, successor)
			}
		})
	}
}

func TestRunStreamedPreservesTransportEvidence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process fixture")
	}
	nativeErr := osExec("sh", "-c", "exit 23").Run()
	processErr, ok := nativeErr.(*osexec.ExitError)
	if !ok || processErr.ExitCode() != 23 {
		t.Fatalf("native exit fixture: %v", nativeErr)
	}
	for _, tc := range []struct {
		name                       string
		code                       int
		err                        error
		transport, cancelAfterExit bool
	}{
		{name: "success"},
		{name: "ordinary exit", code: 23, err: nativeErr},
		{name: "late cancellation", code: 23, err: nativeErr, cancelAfterExit: true},
		{name: "code only", code: 7},
		{name: "joined cancellation", code: 23, err: errors.Join(nativeErr, context.Canceled), transport: true},
		{name: "joined deadline", code: 23, err: errors.Join(nativeErr, context.DeadlineExceeded), transport: true},
		{name: "joined output failure", code: 23, err: errors.Join(nativeErr, io.ErrShortWrite), transport: true},
		{name: "launch failure", code: 1, err: osexec.ErrNotFound, transport: true},
		{name: "output failure", code: 5, err: io.ErrShortWrite, transport: true},
		{name: "mismatched status", code: 17, err: nativeErr, transport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			runner := newRunner(nil, nil)
			runner.contextHook = func(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				if tc.cancelAfterExit {
					cancel()
				}
				return core.LocalCommandResult{ExitCode: tc.code}, tc.err, true
			}
			cli, err := newTensorlakeCLI(newTestConfig(), newTestRuntime(runner))
			if err != nil {
				t.Fatal(err)
			}
			code, err := cli.runStreamed(ctx, []string{"sbx", "exec"}, []string{"fixture"}, io.Discard, io.Discard)
			if code != tc.code || (err != nil) != tc.transport {
				t.Fatalf("code=%d err=%v, want code=%d transport=%t", code, err, tc.code, tc.transport)
			}
			if tc.transport && !errors.Is(err, tc.err) {
				t.Fatalf("returned cause lost: %v", err)
			}
		})
	}
}

type nativeOutcomeWriter struct {
	bytes.Buffer
	cancel context.CancelFunc
	fail   error
}

func (w *nativeOutcomeWriter) Write(data []byte) (int, error) {
	if w.fail != nil {
		return 0, w.fail
	}
	n, err := w.Buffer.Write(data)
	if n > 0 && w.cancel != nil {
		w.cancel()
	}
	return n, err
}

func TestRunNativeCommandOutcomes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX native process fixture")
	}
	for _, scenario := range []string{"success", "exit", "late cancellation", "cancellation", "deadline", "signal", "launch failure", "output failure", "joined output", "joined deadline", "redacted failure"} {
		t.Run(scenario, func(t *testing.T) {
			b, _, runner, claim := ownedTensorlakeFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var stdout nativeOutcomeWriter
			var stderr bytes.Buffer
			b.rt.Stdout, b.rt.Stderr = &stdout, &stderr
			script := "printf native\n"
			wantCode, wantStatus, wantKind := 0, core.RunStatusSucceeded, core.RunErrorNone
			switch scenario {
			case "exit", "late cancellation":
				script += "exit 23\n"
				wantCode, wantStatus, wantKind = 23, core.RunStatusFailed, core.RunErrorCommandExit
			case "cancellation":
				script = "printf 'ready\\n'; exec sleep 30\n"
				stdout.cancel = cancel
				wantCode, wantStatus, wantKind = 1, core.RunStatusCanceled, core.RunErrorCanceled
			case "deadline":
				script = "printf 'ready\\n'; exec sleep 30\n"
				wantCode, wantStatus, wantKind = 1, core.RunStatusTimedOut, core.RunErrorTimeout
			case "signal":
				script = "kill -TERM $$\n"
				wantCode, wantStatus, wantKind = 1, core.RunStatusFailed, core.RunErrorProvider
			case "launch failure", "output failure", "joined output", "redacted failure":
				wantCode, wantStatus, wantKind = 1, core.RunStatusFailed, core.RunErrorProvider
			case "joined deadline":
				wantCode, wantStatus, wantKind = 1, core.RunStatusTimedOut, core.RunErrorTimeout
			}
			if scenario == "joined output" || scenario == "joined deadline" || scenario == "redacted failure" {
				script += "exit 23\n"
			}
			if scenario == "output failure" {
				stdout.fail = io.ErrShortWrite
			}
			program := filepath.Join(t.TempDir(), "native-fixture")
			if scenario != "launch failure" {
				if err := os.WriteFile(program, []byte("#!/bin/sh\n"+script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			b.cfg.Tensorlake.CLIPath = program
			native := core.RuntimeForProviderOperation(io.Discard).Exec
			calls := 0
			var observed error
			runner.contextHook = func(callCtx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				if scriptKey(req.Args) != "sbx exec" || !strings.Contains(strings.Join(req.Args, "\x00"), "__native_outcome__") {
					return core.LocalCommandResult{}, nil, false
				}
				calls++
				if scenario == "deadline" {
					var finish context.CancelFunc
					callCtx, finish = context.WithTimeout(callCtx, 2*time.Second)
					defer finish()
				}
				result, err := native.Run(callCtx, req)
				if scenario == "late cancellation" {
					cancel()
				}
				if scenario == "joined output" {
					err = errors.Join(err, io.ErrShortWrite)
				}
				if scenario == "joined deadline" {
					err = errors.Join(err, context.DeadlineExceeded)
				}
				if scenario == "redacted failure" {
					err = errors.Join(err, errors.New("reflected "+b.cfg.Tensorlake.APIKey))
				}
				observed = err
				return result, err, true
			}
			result, err := b.Run(ctx, RunRequest{ID: claim.LeaseID, Repo: Repo{Root: claim.RepoRoot}, NoSync: true, TimingJSON: true, Command: []string{"__native_outcome__"}})
			if calls != 1 || result.ExitCode != wantCode || result.Status != wantStatus || result.ErrorKind != wantKind || result.Session == nil || !result.Session.Kept || !result.Session.Reused {
				t.Fatalf("scenario=%s calls=%d result=%#v err=%v", scenario, calls, result, err)
			}
			if wantCode == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var public ExitError
				if !errors.As(err, &public) || public.Code != wantCode {
					t.Fatalf("public exit=%#v err=%v", public, err)
				}
				if wantKind != core.RunErrorCommandExit && !errors.Is(err, observed) {
					t.Fatalf("native cause lost: %v", err)
				}
			}
			if scenario == "late cancellation" && errors.Is(err, context.Canceled) {
				t.Fatalf("late cancellation replaced observed exit: %v", err)
			}
			if scenario == "redacted failure" && (strings.Contains(err.Error(), b.cfg.Tensorlake.APIKey) || !strings.Contains(err.Error(), "[redacted]")) {
				t.Fatalf("configured key redaction failed: %v", err)
			}
			if scenario == "cancellation" || scenario == "deadline" {
				if stdout.String() != "ready\n" {
					t.Fatalf("native process did not reach running state: %q", stdout.String())
				}
			}
			var report core.TimingReport
			lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil || report.ExitCode != wantCode || report.RunStatus != wantStatus || report.ErrorKind != wantKind {
				t.Fatalf("timing=%#v err=%v", report, err)
			}
			if got, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || !exists || got.CloudID != claim.CloudID || got.RepoRoot != claim.RepoRoot {
				t.Fatalf("retained claim=%#v exists=%t err=%v", got, exists, err)
			}
			t.Logf("native outcome scenario=%s code=%d status=%s kind=%s kept=%t reused=%t", scenario, result.ExitCode, result.Status, result.ErrorKind, result.Session.Kept, result.Session.Reused)
		})
	}
}

func TestRunSurfacesCommandExitCodeWithoutWrappingError(t *testing.T) {
	testutil.IsolateUserDirs(t)
	runner := newRunner(
		map[string]scriptedReply{
			"sbx create":    {stdout: "abc123def456ghi789000\n"},
			"sbx terminate": {stdout: "abc123def456ghi789000\n"},
		},
		map[string][]scriptedReply{
			// First exec is the mkdir prepare (succeeds); second is the user
			// command (exits 7).
			"sbx exec": {
				{stdout: ""},
				{stderr: "boom\n", exitCode: 7},
			},
		},
	)
	var stderr bytes.Buffer
	rt := newTestRuntime(runner)
	rt.Stderr = &stderr
	backend := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), rt).(*tensorlakeBackend)
	req := RunRequest{
		Repo:       Repo{Name: "carbbox", Root: t.TempDir()},
		Command:    []string{"false"},
		NoSync:     true,
		TimingJSON: true,
	}
	result, err := backend.Run(context.Background(), req)
	if result.ExitCode != 7 {
		t.Fatalf("exit=%d want 7", result.ExitCode)
	}
	if result.Status != core.RunStatusFailed || result.ErrorKind != core.RunErrorCommandExit {
		t.Fatalf("status/error=%q/%q", result.Status, result.ErrorKind)
	}
	if !strings.Contains(stderr.String(), `"runStatus":"failed"`) || !strings.Contains(stderr.String(), `"errorKind":"command-exit"`) {
		t.Fatalf("stderr = %q, want failed command-exit timing", stderr.String())
	}
	var ee ExitError
	if !errors.As(err, &ee) || ee.Code != 7 {
		t.Fatalf("err=%v want ExitError code=7", err)
	}
}

func TestTensorlakeDeleteSyncDoesNotRemoveWorkspaceBeforeUpload(t *testing.T) {
	testutil.IsolateUserDirs(t)
	repoRoot := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(repoRoot, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := newRunner(
		map[string]scriptedReply{
			"sbx create":    {stdout: "syncdelete01234567800\n"},
			"sbx cp":        {exitCode: 7, err: errors.New("upload failed")},
			"sbx terminate": {stdout: "syncdelete01234567800\n"},
		},
		nil,
	)
	cfg := newTestConfig()
	cfg.Sync.Delete = true
	backend := NewTensorlakeBackend(Provider{}.Spec(), cfg, newTestRuntime(runner)).(*tensorlakeBackend)
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "repo", Root: repoRoot},
		Command: []string{"echo", "ok"},
	})
	if err == nil || !strings.Contains(err.Error(), "upload failed") {
		t.Fatalf("err=%v, want upload failure", err)
	}
	cleanup := findCallN(runner, "sbx exec", 0)
	if cleanup == nil {
		t.Fatal("missing failed-upload cleanup")
	}
	cleanupText := strings.Join(cleanup.Args, " ")
	if !strings.Contains(cleanupText, "rm -f '/tmp/crabbox-tensorlake-sync-") || strings.Contains(cleanupText, "rm -rf '/workspace/crabbox'") {
		t.Fatalf("cleanup=%s", cleanupText)
	}
	if verbs := callMutationVerbs(runner); !reflect.DeepEqual(verbs, []string{"sbx create", "sbx cp", "sbx exec", "sbx terminate"}) {
		t.Fatalf("verbs=%v", verbs)
	}
}

func TestTensorlakeSyncNativeArchiveTransaction(t *testing.T) {
	for _, scenario := range []struct {
		name, failure string
		delete        bool
	}{
		{"partial upload", "upload", true}, {"corrupt archive", "extract", true},
		{"replace", "", true}, {"merge", "", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			workdir := filepath.Join(root, "work")
			if err := os.Mkdir(workdir, 0o755); err != nil {
				t.Fatal(err)
			}
			old := filepath.Join(workdir, "old.txt")
			if err := os.WriteFile(old, []byte("preserve-me"), 0o600); err != nil {
				t.Fatal(err)
			}
			runner := newRunner(nil, nil)
			remoteArchive := ""
			t.Cleanup(func() {
				if remoteArchive != "" {
					_ = os.Remove(remoteArchive)
				}
			})
			runner.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				if scriptKey(req.Args) == "sbx cp" {
					src, dst := req.Args[len(req.Args)-2], req.Args[len(req.Args)-1]
					_, remoteArchive, _ = strings.Cut(dst, ":")
					data, err := os.ReadFile(src)
					if err != nil {
						return core.LocalCommandResult{}, err, true
					}
					if scenario.failure == "extract" {
						data = []byte("corrupt archive")
					}
					if err := os.WriteFile(remoteArchive, data, 0o600); err != nil {
						return core.LocalCommandResult{}, err, true
					}
					if scenario.failure == "upload" {
						return core.LocalCommandResult{ExitCode: 7}, errors.New("synthetic partial upload failure"), true
					}
					return core.LocalCommandResult{}, nil, true
				}
				if scriptKey(req.Args) == "sbx exec" {
					cmd := osexec.Command("sh", "-c", req.Args[len(req.Args)-1])
					cmd.Stdout, cmd.Stderr = req.Stdout, req.Stderr
					err := cmd.Run()
					code := 0
					if err != nil {
						var exited *osexec.ExitError
						if !errors.As(err, &exited) {
							return core.LocalCommandResult{}, err, true
						}
						code = exited.ExitCode()
					}
					return core.LocalCommandResult{ExitCode: code}, err, true
				}
				return core.LocalCommandResult{}, nil, false
			}
			cfg := newTestConfig()
			cfg.Sync.Delete = scenario.delete
			backend := NewTensorlakeBackend(Provider{}.Spec(), cfg, newTestRuntime(runner)).(*tensorlakeBackend)
			cli, err := newTensorlakeCLI(cfg, backend.rt)
			if err != nil {
				t.Fatal(err)
			}
			repo := newGitRepo(t)
			if err := os.WriteFile(filepath.Join(repo, "incoming.txt"), []byte("new"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, err = backend.syncWorkspace(context.Background(), cli, "sandbox_fixture", RunRequest{Repo: Repo{Root: repo}}, workdir)
			if scenario.failure != "" && err == nil {
				t.Fatal("expected transfer failure")
			}
			if scenario.failure == "" && err != nil {
				t.Fatal(err)
			}
			if scenario.failure != "" || !scenario.delete {
				data, err := os.ReadFile(old)
				if err != nil || string(data) != "preserve-me" {
					t.Fatalf("previous workspace lost: %q %v", data, err)
				}
			} else if _, err := os.Stat(old); !os.IsNotExist(err) {
				t.Fatalf("old file remains: %v", err)
			}
			if scenario.failure == "" {
				data, err := os.ReadFile(filepath.Join(workdir, "incoming.txt"))
				if err != nil || string(data) != "new" {
					t.Fatalf("incoming=%q err=%v", data, err)
				}
			}
			if _, err := os.Stat(remoteArchive); !os.IsNotExist(err) {
				t.Fatalf("remote archive remains: %v", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Name() != "work" {
					t.Errorf("staging/backup residue %s", e.Name())
				}
			}
		})
	}
}

func TestRunTimingJSONIncludesSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	sandboxID := "timingid0123456789000"
	leaseID := leasePrefix + sandboxID
	defer core.RemoveLeaseClaim(leaseID)
	runner := newRunner(map[string]scriptedReply{
		"sbx create": {stdout: sandboxID + "\n"},
		"sbx exec":   {stdout: "ok\n"},
	}, nil)
	var stderr bytes.Buffer
	rt := newTestRuntime(runner)
	rt.Stderr = &stderr
	backend := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), rt).(*tensorlakeBackend)
	req := RunRequest{
		Repo:       Repo{Name: "carbbox", Root: t.TempDir()},
		Command:    []string{"echo", "ok"},
		NoSync:     true,
		Keep:       true,
		Reclaim:    true,
		TimingJSON: true,
	}
	if _, err := backend.Run(context.Background(), req); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	report := map[string]any{}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.HasPrefix(line, "{") {
			if err := json.Unmarshal([]byte(line), &report); err != nil {
				t.Fatalf("decode timing JSON %q: %v", line, err)
			}
		}
	}
	if report["leaseId"] != leaseID {
		t.Fatalf("leaseId=%v want %s in timing JSON:\n%s", report["leaseId"], leaseID, stderr.String())
	}
	if report["slug"] != newLeaseSlug(leaseID) {
		t.Fatalf("slug=%v want %s in timing JSON:\n%s", report["slug"], newLeaseSlug(leaseID), stderr.String())
	}
}

func TestRunTimingJSONUsesClaimSlugForReusedSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()
	sandboxID := "reuseid01234567890000"
	leaseID := leasePrefix + sandboxID
	if err := claimBoundTensorlakeForTest(leaseID, "custom-slug", repoRoot, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	defer core.RemoveLeaseClaim(leaseID)
	runner := newRunner(map[string]scriptedReply{
		"sbx exec": {stdout: "ok\n"},
	}, nil)
	runner.resources[sandboxID] = sandboxIdentity{ID: sandboxID, Name: "crabbox-fixture", Namespace: "sandbox_ns", State: "running"}
	var stderr bytes.Buffer
	rt := newTestRuntime(runner)
	rt.Stderr = &stderr
	backend := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), rt).(*tensorlakeBackend)
	req := RunRequest{
		ID:         "custom-slug",
		Repo:       Repo{Name: "carbbox", Root: repoRoot},
		Command:    []string{"echo", "ok"},
		NoSync:     true,
		TimingJSON: true,
	}
	if _, err := backend.Run(context.Background(), req); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	report := map[string]any{}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.HasPrefix(line, "{") {
			if err := json.Unmarshal([]byte(line), &report); err != nil {
				t.Fatalf("decode timing JSON %q: %v", line, err)
			}
		}
	}
	if report["slug"] != "custom-slug" {
		t.Fatalf("slug=%v want custom-slug in timing JSON:\n%s", report["slug"], stderr.String())
	}
}

func TestKeepOnFailureRetainsSandboxAndPrintsHint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // keep-on-failure writes a lease claim (and lock); keep both out of the real state dir
	sandboxID := "failkeep0" + randomSuffix() + randomSuffix()
	defer core.RemoveLeaseClaim(leasePrefix + sandboxID)
	runner := newRunner(
		map[string]scriptedReply{
			"sbx create": {stdout: sandboxID + "\n"},
		},
		map[string][]scriptedReply{
			"sbx exec": {
				{stdout: ""},
				{stderr: "boom\n", exitCode: 7},
			},
		},
	)
	var stderr bytes.Buffer
	rt := newTestRuntime(runner)
	rt.Stderr = &stderr
	backend := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), rt).(*tensorlakeBackend)
	req := RunRequest{
		Repo:          Repo{Name: "carbbox", Root: t.TempDir()},
		Command:       []string{"false"},
		NoSync:        true,
		KeepOnFailure: true,
		Reclaim:       true,
	}
	result, err := backend.Run(context.Background(), req)
	if result.ExitCode != 7 {
		t.Fatalf("exit=%d want 7", result.ExitCode)
	}
	var ee ExitError
	if !errors.As(err, &ee) || ee.Code != 7 {
		t.Fatalf("err=%v want ExitError code=7", err)
	}
	if findCall(runner, "sbx terminate") != nil {
		t.Fatalf("sbx terminate called despite --keep-on-failure")
	}
	if !strings.Contains(stderr.String(), "keep-on-failure: kept lease=tlsbx_"+sandboxID) {
		t.Fatalf("missing keep-on-failure hint: %s", stderr.String())
	}
}

func TestRunPerformsArchiveSyncByDefault(t *testing.T) {
	testutil.IsolateUserDirs(t)
	runner := newRunner(map[string]scriptedReply{
		"sbx create":    {stdout: "syncidaaaaaaaaaaaaaa0\n"},
		"sbx exec":      {stdout: "ok\n"},
		"sbx cp":        {stdout: ""},
		"sbx terminate": {stdout: "syncidaaaaaaaaaaaaaa0\n"},
	}, nil)
	backend := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), newTestRuntime(runner)).(*tensorlakeBackend)
	repoRoot := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(repoRoot, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := RunRequest{
		Repo:    Repo{Name: "carbbox", Root: repoRoot},
		Command: []string{"echo", "ok"},
	}
	if _, err := backend.Run(context.Background(), req); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	verbs := callMutationVerbs(runner)
	// Archive upload precedes preparation; its cleanup is separate from user execution.
	want := []string{"sbx create", "sbx cp", "sbx exec", "sbx exec", "sbx exec", "sbx exec", "sbx terminate"}
	if !reflect.DeepEqual(verbs, want) {
		t.Fatalf("verbs=%v want %v", verbs, want)
	}
	cp := findCall(runner, "sbx cp")
	if cp == nil {
		t.Fatalf("missing sbx cp call")
	}
	if !containsArgPrefix(cp.Args, "syncidaaaaaaaaaaaaaa0:/tmp/crabbox-tensorlake-sync-") {
		t.Fatalf("cp args=%v missing remote dest", cp.Args)
	}
}

func TestTensorlakeRunChecksArchiveBeforeAllocation(t *testing.T) {
	runner := newRunner(nil, nil)
	cfg := newTestConfig()
	cfg.Sync.FailFiles = 1
	backend := NewTensorlakeBackend(Provider{}.Spec(), cfg, newTestRuntime(runner)).(*tensorlakeBackend)
	repo := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "incoming.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := backend.Run(context.Background(), RunRequest{Repo: Repo{Root: repo}, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "sync candidate too large") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("provider called before archive admission: %v", runner.calls)
	}
}

func TestRunSkipsSyncWithNoSync(t *testing.T) {
	testutil.IsolateUserDirs(t)
	runner := newRunner(map[string]scriptedReply{
		"sbx create":    {stdout: "nosyncidaaaaaaaaaaaa0\n"},
		"sbx exec":      {stdout: "ok\n"},
		"sbx terminate": {stdout: "nosyncidaaaaaaaaaaaa0\n"},
	}, nil)
	backend := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), newTestRuntime(runner)).(*tensorlakeBackend)
	req := RunRequest{
		Repo:    Repo{Name: "carbbox", Root: t.TempDir()},
		Command: []string{"echo", "ok"},
		NoSync:  true,
	}
	if _, err := backend.Run(context.Background(), req); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if findCall(runner, "sbx cp") != nil {
		t.Fatalf("sbx cp called despite --no-sync")
	}
	verbs := callMutationVerbs(runner)
	// With --no-sync we still prepare the workdir (mkdir) before the user's command.
	want := []string{"sbx create", "sbx exec", "sbx exec", "sbx terminate"}
	if !reflect.DeepEqual(verbs, want) {
		t.Fatalf("verbs=%v want %v", verbs, want)
	}
}

func TestKeepRetainsSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // Keep writes a lease claim; keep it out of the real state dir
	runner := newRunner(map[string]scriptedReply{
		"sbx create": {stdout: "keepid01234567890ab00\n"},
		"sbx exec":   {stdout: "hi\n"},
	}, nil)
	backend := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), newTestRuntime(runner)).(*tensorlakeBackend)
	req := RunRequest{
		Repo:    Repo{Name: "carbbox", Root: t.TempDir()},
		Command: []string{"echo", "hi"},
		NoSync:  true,
		Keep:    true,
		Reclaim: true,
	}
	result, err := backend.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if result.Session == nil {
		t.Fatal("session=nil")
	}
	if result.Session.Provider != providerName || !result.Session.Kept || result.Session.Reused || result.Session.CleanupCommand == "" {
		t.Fatalf("session=%#v", result.Session)
	}
	if findCall(runner, "sbx terminate") != nil {
		t.Fatalf("sbx terminate called despite Keep=true")
	}
}

func TestRunReusedSandboxReportsKeptSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	sandboxID := "reuseidsession1234000"
	leaseID := leasePrefix + sandboxID
	repoRoot := t.TempDir()
	if err := claimBoundTensorlakeForTest(leaseID, "reuse-session", repoRoot, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	defer core.RemoveLeaseClaim(leaseID)
	runner := newRunner(map[string]scriptedReply{
		"sbx exec": {stdout: "hi\n"},
	}, nil)
	runner.resources[sandboxID] = sandboxIdentity{ID: sandboxID, Name: "crabbox-fixture", Namespace: "sandbox_ns", State: "running"}
	backend := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), newTestRuntime(runner)).(*tensorlakeBackend)
	req := RunRequest{
		ID:      "reuse-session",
		Repo:    Repo{Name: "carbbox", Root: repoRoot},
		Command: []string{"echo", "hi"},
		NoSync:  true,
	}
	result, err := backend.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if result.Session == nil {
		t.Fatal("session=nil")
	}
	if result.Session.Provider != providerName || result.Session.LeaseID != leaseID || result.Session.Slug != "reuse-session" || !result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	if findCall(runner, "sbx terminate") != nil {
		t.Fatalf("reused sandbox should not be terminated")
	}
}

func TestRunTerminateFailureReportsRetainedSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := newRunner(
		map[string]scriptedReply{
			"sbx create": {stdout: "termfail0123456789000\n"},
			"sbx exec":   {stdout: "hi\n"},
		},
		map[string][]scriptedReply{
			"sbx terminate": {
				{stderr: "terminate failed\n", exitCode: 1},
			},
		},
	)
	var stderr bytes.Buffer
	rt := newTestRuntime(runner)
	rt.Stderr = &stderr
	backend := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), rt).(*tensorlakeBackend)
	req := RunRequest{
		Repo:    Repo{Name: "carbbox", Root: t.TempDir()},
		Command: []string{"echo", "hi"},
		NoSync:  true,
	}
	result, err := backend.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if result.Session == nil {
		t.Fatal("session=nil")
	}
	if result.Session.Provider != providerName || result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	if !strings.Contains(stderr.String(), "warning: tensorlake terminate failed for termfail0123456789000") {
		t.Fatalf("stderr=%q, want terminate warning", stderr.String())
	}
}

func TestStopRejectsUnclaimedID(t *testing.T) {
	runner := newRunner(nil, nil)
	backend := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), newTestRuntime(runner)).(*tensorlakeBackend)
	err := backend.Stop(context.Background(), StopRequest{ID: "not-claimed-anywhere"})
	if err == nil {
		t.Fatalf("expected rejection of unclaimed sandbox")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("CLI invoked for unclaimed sandbox: %d calls", len(runner.calls))
	}
}

func TestCreateInvocationCarriesSizingFlags(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // Keep writes a lease claim; keep it out of the real state dir
	runner := newRunner(map[string]scriptedReply{
		"sbx create": {stdout: "sizingid0123456789000\n"},
		"sbx exec":   {stdout: "ok\n"},
	}, nil)
	cfg := newTestConfig()
	cfg.Tensorlake.CPUs = 2.5
	cfg.Tensorlake.MemoryMB = 8192
	cfg.Tensorlake.DiskMB = 20000
	cfg.Tensorlake.Image = "ubuntu-22.04"
	cfg.Tensorlake.NoInternet = true
	cfg.Tensorlake.OrganizationID = "org_xyz"
	backend := NewTensorlakeBackend(Provider{}.Spec(), cfg, newTestRuntime(runner)).(*tensorlakeBackend)
	req := RunRequest{
		Repo:    Repo{Name: "carbbox", Root: t.TempDir()},
		Command: []string{"echo", "ok"},
		NoSync:  true,
		Keep:    true,
		Reclaim: true,
	}
	if _, err := backend.Run(context.Background(), req); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	create := findCall(runner, "sbx create")
	if create == nil {
		t.Fatalf("missing sbx create call")
	}
	for _, want := range []string{"-c", "2.5", "-m", "8192", "--disk_mb", "20000", "-i", "ubuntu-22.04", "-N"} {
		if !containsArg(create.Args, want) {
			t.Errorf("create args=%v missing %q", create.Args, want)
		}
	}
	// global flag
	if !containsArg(create.Args, "--organization") || !containsArg(create.Args, "org_xyz") {
		t.Errorf("create args=%v missing --organization org_xyz", create.Args)
	}
}

func callMutationVerbs(r *recordingCommandRunner) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	verbs := make([]string, 0, len(r.calls))
	for _, c := range r.calls {
		if v := scriptKey(c.Args); v != "" && v != "whoami" && v != "sbx describe" {
			verbs = append(verbs, v)
		}
	}
	return verbs
}

func findCall(r *recordingCommandRunner, verb string) *core.LocalCommandRequest {
	return findCallN(r, verb, 0)
}

// findCallN returns the (n+1)-th call to verb (zero-indexed). Returns nil
// when fewer than n+1 calls exist.
func findCallN(r *recordingCommandRunner, verb string, n int) *core.LocalCommandRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := 0
	for i := range r.calls {
		if scriptKey(r.calls[i].Args) == verb {
			if seen == n {
				return &r.calls[i]
			}
			seen++
		}
	}
	return nil
}

// newGitRepo creates a temp directory, runs `git init` + an empty commit so
// `git ls-files` (used by core.BuildSyncManifest) has something to walk.
func newGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", root},
		{"-C", root, "config", "user.email", "test@example.com"},
		{"-C", root, "config", "user.name", "test"},
		{"-C", root, "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := osExec("git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return root
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func containsArgPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

func containsArgSubstring(args []string, needle string) bool {
	for _, a := range args {
		if strings.Contains(a, needle) {
			return true
		}
	}
	return false
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}
