package tensorlake

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
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

func TestBuildCommandAutoWrapsShellMetacharacters(t *testing.T) {
	got, err := buildCommand([]string{"pnpm install && pnpm test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "bash" || got[1] != "-lc" {
		t.Fatalf("command=%#v want bash -lc wrapping", got)
	}
	if !strings.Contains(got[2], "pnpm install") || !strings.Contains(got[2], "pnpm test") {
		t.Fatalf("command=%#v missing user input", got)
	}
}

func TestBuildCommandAutoWrapsLeadingEnvAssignment(t *testing.T) {
	got, err := buildCommand([]string{"FOO=bar", "pnpm", "test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "bash" {
		t.Fatalf("command=%#v want bash wrapping for FOO=bar", got)
	}
}

func TestTensorlakeWorkdirRejectsRelative(t *testing.T) {
	cfg := newTestConfig()
	cfg.Tensorlake.Workdir = "relative/path"
	if _, err := tensorlakeWorkdir(cfg); err == nil {
		t.Fatalf("expected rejection of relative workdir")
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

func TestBuildCommandShellMode(t *testing.T) {
	got, err := buildCommand([]string{"pnpm install && pnpm test"}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bash", "-lc", "pnpm install && pnpm test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command=%#v want %#v", got, want)
	}
}

func TestBuildCommandPassThrough(t *testing.T) {
	got, err := buildCommand([]string{"pnpm", "test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pnpm", "test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command=%#v want %#v", got, want)
	}
}

func TestBuildCommandRejectsEmpty(t *testing.T) {
	if _, err := buildCommand(nil, false); err == nil {
		t.Fatalf("expected error for empty command")
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

func TestParseDescribeStateExtractsStatus(t *testing.T) {
	out := strings.Join([]string{
		"ID:              3pryjysezwsnlex226i5h",
		"Name:            crabbox-app-aaa111",
		"Status:          running",
		"Image:           ubuntu-minimal",
	}, "\n")
	if got := parseDescribeState(out); got != "running" {
		t.Fatalf("state=%q want running", got)
	}
	if got := parseDescribeState(""); got != "" {
		t.Fatalf("empty input should return empty, got %q", got)
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
	_, _, err := resolveLeaseID("not-a-known-slug", "", false, 0)
	if err == nil || !strings.Contains(err.Error(), "not claimed by Crabbox") {
		t.Fatalf("err=%v, want rejection of unclaimed sandbox", err)
	}
}

func TestResolveLeaseIDRejectsLeasePrefixWithoutClaim(t *testing.T) {
	_, _, err := resolveLeaseID("tlsbx_unknown123", "", false, 0)
	if err == nil || !strings.Contains(err.Error(), "not claimed by Crabbox") {
		t.Fatalf("err=%v, want rejection without local claim", err)
	}
}

func TestResolveLeaseIDRequiresIdentifier(t *testing.T) {
	if _, _, err := resolveLeaseID("", "", false, 0); err == nil {
		t.Fatalf("expected error for empty id")
	}
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
	mu       sync.Mutex
	calls    []core.LocalCommandRequest
	scripts  map[string][]scriptedReply
	defaults map[string]scriptedReply
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
	res := core.LocalCommandResult{
		ExitCode: reply.exitCode,
		Stdout:   reply.stdout,
		Stderr:   reply.stderr,
	}
	return res, reply.err
}

func newRunner(defaults map[string]scriptedReply, sequenced map[string][]scriptedReply) *recordingCommandRunner {
	return &recordingCommandRunner{defaults: defaults, scripts: sequenced}
}

// scriptKey extracts the `sbx <verb>` portion of an argv slice, ignoring
// global flags so test scripts can match by subcommand alone.
func scriptKey(args []string) string {
	for i, a := range args {
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
	verbs := callVerbs(runner)
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
		t.Fatalf("env missing TENSORLAKE_API_KEY: %v", execCall.Env)
	}
}

func TestRunForwardsEnvViaUploadedProfile(t *testing.T) {
	runner := newRunner(map[string]scriptedReply{
		"sbx create":    {stdout: "envid0123456789012\n"},
		"sbx exec":      {stdout: "ok\n"},
		"sbx cp":        {stdout: ""},
		"sbx terminate": {stdout: "envid0123456789012\n"},
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
	verbs := callVerbs(runner)
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

func TestRunSurfacesCommandExitCodeWithoutWrappingError(t *testing.T) {
	exitErr := &fakeExitError{code: 7}
	runner := newRunner(
		map[string]scriptedReply{
			"sbx create":    {stdout: "abc123def456ghi789\n"},
			"sbx terminate": {stdout: "abc123def456ghi789\n"},
		},
		map[string][]scriptedReply{
			// First exec is the mkdir prepare (succeeds); second is the user
			// command (exits 7).
			"sbx exec": {
				{stdout: ""},
				{stderr: "boom\n", exitCode: 7, err: exitErr},
			},
		},
	)
	backend := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), newTestRuntime(runner)).(*tensorlakeBackend)
	req := RunRequest{
		Repo:    Repo{Name: "carbbox", Root: t.TempDir()},
		Command: []string{"false"},
		NoSync:  true,
	}
	result, err := backend.Run(context.Background(), req)
	if result.ExitCode != 7 {
		t.Fatalf("exit=%d want 7", result.ExitCode)
	}
	var ee ExitError
	if !errors.As(err, &ee) || ee.Code != 7 {
		t.Fatalf("err=%v want ExitError code=7", err)
	}
}

func TestKeepOnFailureRetainsSandboxAndPrintsHint(t *testing.T) {
	sandboxID := "failkeep" + randomSuffix() + randomSuffix()
	defer removeLeaseClaim(leasePrefix + sandboxID)
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
	runner := newRunner(map[string]scriptedReply{
		"sbx create":    {stdout: "syncidaaaaaaaaaaaaaa\n"},
		"sbx exec":      {stdout: "ok\n"},
		"sbx cp":        {stdout: ""},
		"sbx terminate": {stdout: "syncidaaaaaaaaaaaaaa\n"},
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
	verbs := callVerbs(runner)
	// Expected order: create → mkdir-prepare exec → cp upload → tar-extract exec → user exec → terminate
	want := []string{"sbx create", "sbx exec", "sbx cp", "sbx exec", "sbx exec", "sbx terminate"}
	if !reflect.DeepEqual(verbs, want) {
		t.Fatalf("verbs=%v want %v", verbs, want)
	}
	cp := findCall(runner, "sbx cp")
	if cp == nil {
		t.Fatalf("missing sbx cp call")
	}
	if !containsArgPrefix(cp.Args, "syncidaaaaaaaaaaaaaa:/tmp/crabbox-sync-") {
		t.Fatalf("cp args=%v missing remote dest", cp.Args)
	}
}

func TestRunSkipsSyncWithNoSync(t *testing.T) {
	runner := newRunner(map[string]scriptedReply{
		"sbx create":    {stdout: "nosyncidaaaaaaaaaaaa\n"},
		"sbx exec":      {stdout: "ok\n"},
		"sbx terminate": {stdout: "nosyncidaaaaaaaaaaaa\n"},
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
	verbs := callVerbs(runner)
	// With --no-sync we still prepare the workdir (mkdir) before the user's command.
	want := []string{"sbx create", "sbx exec", "sbx exec", "sbx terminate"}
	if !reflect.DeepEqual(verbs, want) {
		t.Fatalf("verbs=%v want %v", verbs, want)
	}
}

func TestKeepRetainsSandbox(t *testing.T) {
	runner := newRunner(map[string]scriptedReply{
		"sbx create": {stdout: "keepid01234567890ab\n"},
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
	if _, err := backend.Run(context.Background(), req); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if findCall(runner, "sbx terminate") != nil {
		t.Fatalf("sbx terminate called despite Keep=true")
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
	runner := newRunner(map[string]scriptedReply{
		"sbx create": {stdout: "sizingid01234567890\n"},
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

func callVerbs(r *recordingCommandRunner) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	verbs := make([]string, 0, len(r.calls))
	for _, c := range r.calls {
		if v := scriptKey(c.Args); v != "" {
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

type fakeExitError struct{ code int }

func (e *fakeExitError) Error() string { return "exit" }
func (e *fakeExitError) ExitCode() int { return e.code }
