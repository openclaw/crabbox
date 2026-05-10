package tensorlake

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

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

// recordingCommandRunner is a fake CommandRunner that records every call and
// replies with a scripted (stdout, stderr, exit, err) tuple keyed by the
// subcommand sequence (e.g. "sbx create", "sbx exec", "sbx terminate").
type recordingCommandRunner struct {
	mu      sync.Mutex
	calls   []core.LocalCommandRequest
	scripts map[string]scriptedReply
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
	r.mu.Unlock()
	key := scriptKey(req.Args)
	reply := r.scripts[key]
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
	runner := &recordingCommandRunner{
		scripts: map[string]scriptedReply{
			"sbx create":    {stdout: "3pryjysezwsnlex226i5h\n"},
			"sbx exec":      {stdout: "hello\n"},
			"sbx terminate": {stdout: "3pryjysezwsnlex226i5h\n"},
		},
	}
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
	want := []string{"sbx create", "sbx exec", "sbx terminate"}
	if !reflect.DeepEqual(verbs, want) {
		t.Fatalf("verbs=%v want %v", verbs, want)
	}
	// `sbx exec` must target the captured sandbox ID, not the human name.
	execCall := findCall(runner, "sbx exec")
	if execCall == nil {
		t.Fatalf("missing sbx exec call")
	}
	if !containsArg(execCall.Args, "3pryjysezwsnlex226i5h") {
		t.Fatalf("exec args=%v missing sandbox id", execCall.Args)
	}
	if !containsArg(execCall.Args, "echo") || !containsArg(execCall.Args, "hello") {
		t.Fatalf("exec args=%v missing user command", execCall.Args)
	}
	// API key must flow via env, never argv.
	if containsArgPrefix(execCall.Args, "tl_apiKey_") {
		t.Fatalf("API key leaked into argv: %v", execCall.Args)
	}
	if !containsEnv(execCall.Env, "TENSORLAKE_API_KEY=tl_apiKey_test") {
		t.Fatalf("env missing TENSORLAKE_API_KEY: %v", execCall.Env)
	}
}

func TestRunSurfacesCommandExitCodeWithoutWrappingError(t *testing.T) {
	exitErr := &fakeExitError{code: 7}
	runner := &recordingCommandRunner{
		scripts: map[string]scriptedReply{
			"sbx create":    {stdout: "abc123def456ghi789\n"},
			"sbx exec":      {stderr: "boom\n", exitCode: 7, err: exitErr},
			"sbx terminate": {stdout: "abc123def456ghi789\n"},
		},
	}
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

func TestRunRequiresNoSync(t *testing.T) {
	runner := &recordingCommandRunner{}
	backend := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), newTestRuntime(runner)).(*tensorlakeBackend)
	req := RunRequest{
		Repo:    Repo{Name: "carbbox", Root: t.TempDir()},
		Command: []string{"echo"},
		NoSync:  false,
	}
	if _, err := backend.Run(context.Background(), req); err == nil {
		t.Fatalf("expected error when --no-sync is missing")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("CLI was invoked despite sync rejection: %d calls", len(runner.calls))
	}
}

func TestKeepRetainsSandbox(t *testing.T) {
	runner := &recordingCommandRunner{
		scripts: map[string]scriptedReply{
			"sbx create": {stdout: "keepid01234567890ab\n"},
			"sbx exec":   {stdout: "hi\n"},
		},
	}
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
	runner := &recordingCommandRunner{}
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
	runner := &recordingCommandRunner{
		scripts: map[string]scriptedReply{
			"sbx create": {stdout: "sizingid01234567890\n"},
			"sbx exec":   {stdout: "ok\n"},
		},
	}
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
