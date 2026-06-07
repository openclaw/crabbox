package dockersandbox

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"math"
	"os"
	"os/exec"
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

func TestProviderWrappersConfigureBackendAndDoctor(t *testing.T) {
	provider := Provider{}
	cfg := newTestConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := provider.RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{"--docker-sandbox-cli", "/opt/sbx", "--docker-sandbox-memory", "8g"}); err != nil {
		t.Fatal(err)
	}
	if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatalf("ApplyFlags err=%v", err)
	}
	if cfg.DockerSandbox.CLIPath != "/opt/sbx" || cfg.DockerSandbox.Memory != "8g" {
		t.Fatalf("cfg=%#v", cfg.DockerSandbox)
	}

	rt := Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: newRunner(nil, nil)}
	configured, err := provider.Configure(cfg, rt)
	if err != nil {
		t.Fatalf("Configure err=%v", err)
	}
	if configured.Spec().Name != providerName {
		t.Fatalf("backend spec=%#v", configured.Spec())
	}
	doctor, err := provider.ConfigureDoctor(cfg, rt)
	if err != nil {
		t.Fatalf("ConfigureDoctor err=%v", err)
	}
	if _, ok := doctor.(*backend); !ok {
		t.Fatalf("doctor backend type=%T", doctor)
	}

	badCfg := cfg
	badCfg.DockerSandbox.Agent = "codex"
	if _, err := provider.Configure(badCfg, rt); err == nil || !strings.Contains(err.Error(), "v1 supports shell only") {
		t.Fatalf("Configure invalid err=%v", err)
	}
}

func TestParseSandboxListToleratesArraysAndWrappers(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantID    string
		wantName  string
		wantState string
		wantAgent string
		wantWork  string
	}{
		{
			name:      "top level array canonical fields",
			input:     `[{"id":"abc","name":"crabbox-my-app-123abc","status":"running","agent":"shell","workspace":"/workspace"}]`,
			wantID:    "abc",
			wantName:  "crabbox-my-app-123abc",
			wantState: "running",
			wantAgent: "shell",
			wantWork:  "/workspace",
		},
		{
			name:      "sandboxes wrapper snake case aliases",
			input:     `{"sandboxes":[{"sandbox_id":"snake-id","sandbox_name":"crabbox-my-app-snake","state":"ready","working_dir":"/snake"}]}`,
			wantID:    "snake-id",
			wantName:  "crabbox-my-app-snake",
			wantState: "ready",
			wantWork:  "/snake",
		},
		{
			name:      "items wrapper title case aliases",
			input:     `{"items":[{"ID":"title-id","Name":"crabbox-my-app-title","Status":"Started","Agent":"shell"}]}`,
			wantID:    "title-id",
			wantName:  "crabbox-my-app-title",
			wantState: "started",
			wantAgent: "shell",
		},
		{
			name:      "data wrapper camel case aliases",
			input:     `{"data":[{"sandboxId":"camel-id","sandboxName":"crabbox-my-app-camel","status":"ACTIVE","workingDir":"/camel"}]}`,
			wantID:    "camel-id",
			wantName:  "crabbox-my-app-camel",
			wantState: "active",
			wantWork:  "/camel",
		},
		{
			name:      "results wrapper workdir alias",
			input:     `{"results":[{"id":"results-id","name":"crabbox-my-app-results","state":"provisioning","workdir":"/results"}]}`,
			wantID:    "results-id",
			wantName:  "crabbox-my-app-results",
			wantState: "provisioning",
			wantWork:  "/results",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records, err := parseSandboxList(tt.input)
			if err != nil {
				t.Fatalf("parseSandboxList(%s): %v", tt.input, err)
			}
			if len(records) != 1 {
				t.Fatalf("records=%#v want one", records)
			}
			got := records[0]
			if got.ID != tt.wantID || got.Name != tt.wantName || got.State != tt.wantState || got.Agent != tt.wantAgent || got.Workspace != tt.wantWork {
				t.Fatalf("record=%#v want id=%q name=%q state=%q agent=%q workspace=%q", got, tt.wantID, tt.wantName, tt.wantState, tt.wantAgent, tt.wantWork)
			}
		})
	}
}

func TestParseSandboxListCoercesFieldsAndRejectsInvalidShapes(t *testing.T) {
	records, err := parseSandboxList(`{"data":[{"id":42,"name":true,"status":"READY","agent":false,"workspace":"/repo"}, "ignored", {}]}`)
	if err != nil {
		t.Fatalf("parseSandboxList coercion: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records=%#v want one usable record", records)
	}
	if records[0].ID != "42" || records[0].Name != "true" || records[0].State != "ready" || records[0].Agent != "false" || records[0].Workspace != "/repo" {
		t.Fatalf("record=%#v", records[0])
	}
	if records, err := parseSandboxList(""); err != nil || len(records) != 0 {
		t.Fatalf("empty parse records=%#v err=%v", records, err)
	}
	if _, err := parseSandboxList(`42`); err == nil || !strings.Contains(err.Error(), "expected array or object") {
		t.Fatalf("scalar parse err=%v", err)
	}
	if _, err := parseSandboxList(`{`); err == nil || !strings.Contains(err.Error(), "parse sbx ls --json") {
		t.Fatalf("invalid json err=%v", err)
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
	if containsArg(create.Args, "--cpus") {
		t.Fatalf("create args=%v should omit --cpus for default zero value", create.Args)
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

func TestRunCloneModeKeepsSandboxAfterSuccess(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := newTestConfig()
	cfg.DockerSandbox.Clone = true
	repoRoot := t.TempDir()
	runGit(t, repoRoot, "init", "-q")
	var stderr bytes.Buffer
	runner := newRunner(map[string]scriptedReply{
		"create": {stdout: ""},
		"exec":   {stdout: "ok\n"},
		"rm":     {stdout: ""},
	}, nil)
	backend := newTestBackend(cfg, runner, io.Discard, &stderr)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: repoRoot},
		Command: []string{"echo", "ok"},
	})
	if err != nil {
		t.Fatalf("Run err=%v stderr=%s", err, stderr.String())
	}
	if result.Session == nil || !result.Session.Kept {
		t.Fatalf("session=%#v, want clone-mode sandbox kept after success", result.Session)
	}
	if got, want := callVerbs(runner), []string{"create", "exec"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("verbs=%v want kept sandbox without rm %v", got, want)
	}
	if !strings.Contains(stderr.String(), "clone run kept sandbox") || !strings.Contains(stderr.String(), result.Session.CleanupCommand) {
		t.Fatalf("stderr missing clone cleanup guidance: %s", stderr.String())
	}
	if claim, ok, err := resolveLeaseClaimForProvider(result.LeaseID, providerName); err != nil || !ok || claim.LeaseID == "" {
		t.Fatalf("kept clone claim claim=%#v ok=%t err=%v", claim, ok, err)
	}
}

func TestRunBuildsConfiguredCreateCommandAndExec(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := newTestConfig()
	cfg.DockerSandbox.Template = "ubuntu"
	cfg.DockerSandbox.CPUs = 2
	cfg.DockerSandbox.Memory = "6g"
	cfg.DockerSandbox.MCP = []string{"context7", "all"}
	cfg.DockerSandbox.Kit = []string{"example-org/base"}
	cfg.DockerSandbox.Clone = true
	cfg.DockerSandbox.ExtraWorkspaces = []string{"/tmp/extra"}
	cfg.DockerSandbox.Workdir = "/workspace/my-app"
	repoRoot := t.TempDir()
	runGit(t, repoRoot, "init", "-q")
	runner := newRunner(map[string]scriptedReply{
		"create": {stdout: ""},
		"exec":   {stdout: "ok\n"},
		"rm":     {stdout: ""},
	}, nil)
	backend := newTestBackend(cfg, runner, io.Discard, io.Discard)
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:      Repo{Name: "my-app", Root: repoRoot},
		Command:   []string{"echo", "hello"},
		ShellMode: true,
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	create := findCall(runner, "create")
	if create == nil {
		t.Fatal("missing create call")
	}
	for _, want := range []string{"--template", "ubuntu", "--cpus", "2", "--memory", "6g", "--mcp", "context7", "all", "--kit", "example-org/base", "--clone", "shell", repoRoot, "/tmp/extra"} {
		if !containsArg(create.Args, want) {
			t.Fatalf("create args=%v missing %q", create.Args, want)
		}
	}
	execCall := findCall(runner, "exec")
	if execCall == nil {
		t.Fatal("missing exec call")
	}
	for _, want := range []string{"--workdir", "/workspace/my-app", "sh", "-lc"} {
		if !containsArg(execCall.Args, want) {
			t.Fatalf("exec args=%v missing %q", execCall.Args, want)
		}
	}
	if got := strings.Join(execCall.Args, " "); strings.Contains(got, "GREETING=") || strings.Contains(got, " exec ") {
		t.Fatalf("exec args=%v should not include env wrapper", execCall.Args)
	}
}

func TestRunForwardsEnvViaSBXEnvFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stderr bytes.Buffer
	runner := newRunner(map[string]scriptedReply{
		"create": {stdout: ""},
		"exec":   {stdout: "ok\n"},
		"rm":     {stdout: ""},
	}, nil)
	backend := newTestBackend(newTestConfig(), runner, io.Discard, &stderr)
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:       Repo{Name: "my-app", Root: t.TempDir()},
		Command:    []string{"printenv", "SECRET_TOKEN"},
		Env:        map[string]string{"SECRET_TOKEN": "secret-token-value"},
		EnvSummary: true,
		Options:    core.LeaseOptions{EnvAllow: []string{"SECRET_TOKEN"}},
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	out := stderr.String()
	for _, want := range []string{"provider=docker-sandbox", "behavior=forwarded", "SECRET_TOKEN=set len=18 secret=true"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "secret-token-value") {
		t.Fatalf("stderr leaked env value: %q", out)
	}
	execCall := findCall(runner, "exec")
	if execCall == nil {
		t.Fatal("missing exec call")
	}
	if !containsArg(execCall.Args, "--env-file") {
		t.Fatalf("exec args=%v missing --env-file", execCall.Args)
	}
	if strings.Contains(strings.Join(execCall.Args, " "), "secret-token-value") {
		t.Fatalf("secret leaked in exec argv: %v", execCall.Args)
	}
	if strings.Contains(strings.Join(execCall.Args, " "), "SECRET_TOKEN=secret-token-value") {
		t.Fatalf("env assignment leaked in exec argv: %v", execCall.Args)
	}
}

func TestRunCleansUpEnvFileAfterExecFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := newRunner(map[string]scriptedReply{
		"create": {stdout: ""},
		"exec":   {exitCode: 7, stderr: "failed\n"},
		"rm":     {stdout: ""},
	}, nil)
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: t.TempDir()},
		Command: []string{"printenv", "SECRET_TOKEN"},
		Env:     map[string]string{"SECRET_TOKEN": "secret-token-value"},
	})
	var exitErr core.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("Run err=%v want exit 7", err)
	}
	execCall := findCall(runner, "exec")
	if execCall == nil {
		t.Fatal("missing exec call")
	}
	envFile := ""
	for i, arg := range execCall.Args {
		if arg == "--env-file" && i+1 < len(execCall.Args) {
			envFile = execCall.Args[i+1]
			break
		}
	}
	if envFile == "" {
		t.Fatalf("exec args=%v missing --env-file path", execCall.Args)
	}
	if _, err := os.Stat(envFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("env file %q still exists or unexpected stat err=%v", envFile, err)
	}
}

func TestFormatDockerSandboxEnvFile(t *testing.T) {
	got, err := formatDockerSandboxEnvFile(map[string]string{
		"Z_FLAG":       "last",
		"SECRET_TOKEN": "secret value",
	})
	if err != nil {
		t.Fatalf("formatDockerSandboxEnvFile err=%v", err)
	}
	if got != "SECRET_TOKEN=secret value\nZ_FLAG=last\n" {
		t.Fatalf("env file=%q", got)
	}
	if _, err := formatDockerSandboxEnvFile(map[string]string{"BAD-NAME": "x"}); err == nil || !strings.Contains(err.Error(), "valid shell environment name") {
		t.Fatalf("bad name err=%v", err)
	}
	if _, err := formatDockerSandboxEnvFile(map[string]string{"SECRET_TOKEN": "line\nbreak"}); err == nil || !strings.Contains(err.Error(), "newlines") {
		t.Fatalf("newline err=%v", err)
	}
	if _, err := formatDockerSandboxEnvFile(map[string]string{"SECRET_TOKEN": "carriage\rreturn"}); err == nil || !strings.Contains(err.Error(), "newlines") {
		t.Fatalf("carriage return err=%v", err)
	}
	if !validDockerSandboxEnvName("_OK_1") || validDockerSandboxEnvName("1_BAD") || validDockerSandboxEnvName("BAD.NAME") || validDockerSandboxEnvName("") {
		t.Fatal("validDockerSandboxEnvName accepted or rejected the wrong names")
	}
}

func TestWriteDockerSandboxEnvFileCreatesAndCleansUpFile(t *testing.T) {
	path, cleanup, err := writeDockerSandboxEnvFile(map[string]string{"SECRET_TOKEN": "secret value"})
	if err != nil {
		t.Fatalf("writeDockerSandboxEnvFile err=%v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read env file err=%v", err)
	}
	if string(data) != "SECRET_TOKEN=secret value\n" {
		t.Fatalf("env file body=%q", data)
	}
	cleanup()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("env file still exists or unexpected stat err=%v", err)
	}
}

func TestRunEnvSummaryTimingAndNoEnvBranches(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stderr bytes.Buffer
	runner := newRunner(map[string]scriptedReply{
		"create": {stdout: ""},
		"exec":   {stdout: "ok\n"},
		"rm":     {stdout: ""},
	}, nil)
	backend := newTestBackend(newTestConfig(), runner, io.Discard, &stderr)
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:       Repo{Name: "my-app", Root: t.TempDir()},
		Command:    []string{"true"},
		EnvSummary: true,
		TimingJSON: true,
	})
	if err != nil {
		t.Fatalf("Run env summary err=%v", err)
	}
	if !strings.Contains(stderr.String(), "env forwarding") || !strings.Contains(stderr.String(), `"provider":"docker-sandbox"`) {
		t.Fatalf("stderr=%s missing env summary or timing JSON", stderr.String())
	}
	execCall := findCall(runner, "exec")
	if execCall == nil {
		t.Fatal("missing exec call")
	}
	if strings.Contains(strings.Join(execCall.Args, " "), " exec ") {
		t.Fatalf("exec args=%v should not be env-wrapped without env values", execCall.Args)
	}
	if containsArg(execCall.Args, "sh") || containsArg(execCall.Args, "-lc") {
		t.Fatalf("exec args=%v should pass command directly without env values", execCall.Args)
	}

	t.Setenv("CRABBOX_ENV_ALLOW", "PATH")
	stderr.Reset()
	runner = newRunner(map[string]scriptedReply{
		"create": {stdout: ""},
		"exec":   {stdout: "ok\n"},
		"rm":     {stdout: ""},
	}, nil)
	backend = newTestBackend(newTestConfig(), runner, io.Discard, &stderr)
	_, err = backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: t.TempDir()},
		Command: []string{"true"},
	})
	if err != nil {
		t.Fatalf("Run env allow summary err=%v", err)
	}
	if !strings.Contains(stderr.String(), "env forwarding") {
		t.Fatalf("stderr=%s missing CRABBOX_ENV_ALLOW summary", stderr.String())
	}

	runner = newRunner(map[string]scriptedReply{
		"create": {stdout: ""},
		"exec":   {stdout: "ok\n"},
		"rm":     {stdout: ""},
	}, nil)
	backend = newTestBackend(newTestConfig(), runner, io.Discard, errWriter{})
	_, err = backend.Run(context.Background(), RunRequest{
		Repo:       Repo{Name: "my-app", Root: t.TempDir()},
		Command:    []string{"true"},
		TimingJSON: true,
	})
	if err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("timing writer err=%v", err)
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

func TestRunWithExistingIDClassifiesMissingSBXCLI(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()
	leaseID := leasePrefix + "crabbox-my-app-missing-cli"
	if err := claimLeaseForRepoProviderPond(leaseID, "missing-cli", providerName, "", repoRoot, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	runner := newRunner(map[string]scriptedReply{
		"exec": {stderr: "not found", exitCode: 1, err: os.ErrNotExist},
	}, nil)
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: repoRoot},
		ID:      "missing-cli",
		Command: []string{"pwd"},
	})
	if err == nil || !strings.Contains(err.Error(), "install the Docker Sandbox sbx CLI") {
		t.Fatalf("Run missing sbx err=%v", err)
	}
}

func TestRunKeepsClaimOnKeepAndCleansUpAfterCommandBuildFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := newRunner(map[string]scriptedReply{
		"create": {stdout: ""},
		"exec":   {stdout: "ok\n"},
		"rm":     {stdout: ""},
	}, nil)
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: t.TempDir()},
		Command: []string{"true"},
		Keep:    true,
	})
	if err != nil {
		t.Fatalf("Run keep err=%v", err)
	}
	if _, ok, err := resolveLeaseClaimForProvider(result.LeaseID, providerName); err != nil || !ok {
		t.Fatalf("kept claim missing ok=%t err=%v", ok, err)
	}

	runner = newRunner(map[string]scriptedReply{"create": {stdout: ""}, "rm": {stdout: ""}}, nil)
	backend = newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	_, err = backend.Run(context.Background(), RunRequest{Repo: Repo{Name: "my-app", Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "missing command") {
		t.Fatalf("Run empty command err=%v", err)
	}
	if got, want := callVerbs(runner), []string{"create", "rm"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("verbs=%v want cleanup after command-build failure %v", got, want)
	}
}

func TestCreateSandboxRemovesSandboxWhenClaimSetupFails(t *testing.T) {
	repoRoot := t.TempDir()
	oldRandomBytes := randomBytes
	defer func() { randomBytes = oldRandomBytes }()
	randomBytes = func(b []byte) (int, error) {
		for i := range b {
			b[i] = byte(i + 1)
		}
		return len(b), nil
	}

	for _, tt := range []struct {
		name          string
		requestedSlug string
		setupState    func(t *testing.T)
		want          string
	}{
		{
			name:          "slug allocation",
			requestedSlug: "wanted",
			setupState: func(t *testing.T) {
				stateFile := filepathJoin(t.TempDir(), "state-file")
				if err := os.WriteFile(stateFile, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("XDG_STATE_HOME", stateFile)
			},
			want: "read claims directory",
		},
		{
			name:          "claim persistence",
			requestedSlug: "",
			setupState: func(t *testing.T) {
				stateDir := t.TempDir()
				claimsDir := filepathJoin(stateDir, "crabbox", "claims")
				if err := os.MkdirAll(claimsDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(claimsDir, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(claimsDir, 0o700) })
				t.Setenv("XDG_STATE_HOME", stateDir)
			},
			want: "write claim",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.setupState(t)
			runner := newRunner(map[string]scriptedReply{
				"create": {stdout: ""},
				"rm":     {stdout: ""},
			}, nil)
			backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
			cli := &sbxCLI{cfg: backend.cfg, rt: backend.rt}
			_, _, _, err := backend.createSandbox(context.Background(), cli, Repo{Name: "my-app", Root: repoRoot}, false, tt.requestedSlug)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("createSandbox err=%v want %q", err, tt.want)
			}
			if got, want := callVerbs(runner), []string{"create", "rm"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("verbs=%v want cleanup verbs %v", got, want)
			}
			rm := findCall(runner, "rm")
			if rm == nil || !reflect.DeepEqual(rm.Args, []string{"rm", "--force", "crabbox-my-app-010203"}) {
				t.Fatalf("rm args=%v", rm.Args)
			}
		})
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

func TestStatusReadyMissingWaitAndTimeout(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := leasePrefix + "crabbox-my-app-status"
	if err := claimLeaseForRepoProviderPond(leaseID, "status", providerName, "", t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	readyRunner := newRunner(map[string]scriptedReply{
		"ls": {stdout: `[{"name":"crabbox-my-app-status","status":"running","agent":"shell","workspace":"/repo"}]`},
	}, nil)
	view, err := newTestBackend(newTestConfig(), readyRunner, io.Discard, io.Discard).Status(context.Background(), StatusRequest{ID: "status"})
	if err != nil {
		t.Fatalf("Status ready err=%v", err)
	}
	if !view.Ready || view.ServerType != providerName || view.Labels["workspace"] != "/repo" {
		t.Fatalf("view=%#v", view)
	}

	missingRunner := newRunner(map[string]scriptedReply{"ls": {stdout: `[]`}}, nil)
	_, err = newTestBackend(newTestConfig(), missingRunner, io.Discard, io.Discard).Status(context.Background(), StatusRequest{ID: "status"})
	if err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("missing status err=%v", err)
	}

	terminalRunner := newRunner(map[string]scriptedReply{
		"ls": {stdout: `[{"name":"crabbox-my-app-status","status":"stopped"}]`},
	}, nil)
	view, err = newTestBackend(newTestConfig(), terminalRunner, io.Discard, io.Discard).Status(context.Background(), StatusRequest{
		ID:          "status",
		Wait:        true,
		WaitTimeout: time.Nanosecond,
	})
	if err != nil || view.Ready || view.State != "stopped" {
		t.Fatalf("terminal stopped status view=%#v err=%v", view, err)
	}

	oldPoll := statusPollInterval
	statusPollInterval = time.Nanosecond
	defer func() { statusPollInterval = oldPoll }()
	waitRunner := newRunner(nil, map[string][]scriptedReply{
		"ls": {
			{stdout: `[{"name":"crabbox-my-app-status","status":"provisioning"}]`},
			{stdout: `[{"name":"crabbox-my-app-status","status":"running"}]`},
		},
	})
	view, err = newTestBackend(newTestConfig(), waitRunner, io.Discard, io.Discard).Status(context.Background(), StatusRequest{
		ID:          "status",
		Wait:        true,
		WaitTimeout: time.Second,
	})
	if err != nil || !view.Ready {
		t.Fatalf("wait ready view=%#v err=%v", view, err)
	}

	timeoutRunner := newRunner(map[string]scriptedReply{
		"ls": {stdout: `[{"name":"crabbox-my-app-status","status":"provisioning"}]`},
	}, nil)
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer timeoutCancel()
	_, err = newTestBackend(newTestConfig(), timeoutRunner, io.Discard, io.Discard).Status(timeoutCtx, StatusRequest{
		ID:          "status",
		Wait:        true,
		WaitTimeout: time.Nanosecond,
	})
	if err == nil || !strings.Contains(err.Error(), "timed out waiting") {
		t.Fatalf("timeout status err=%v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err = newTestBackend(newTestConfig(), timeoutRunner, io.Discard, io.Discard).Status(ctx, StatusRequest{
		ID:   "status",
		Wait: true,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("default wait context err=%v", err)
	}
}

func TestStatusWaitTimeoutDoesNotSleepPastDeadline(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := leasePrefix + "crabbox-my-app-short-timeout"
	if err := claimLeaseForRepoProviderPond(leaseID, "short-timeout", providerName, "", t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	oldPoll := statusPollInterval
	statusPollInterval = 200 * time.Millisecond
	defer func() { statusPollInterval = oldPoll }()

	runner := newRunner(map[string]scriptedReply{
		"ls": {stdout: `[{"name":"crabbox-my-app-short-timeout","status":"provisioning"}]`},
	}, nil)
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)

	start := time.Now()
	_, err := backend.Status(context.Background(), StatusRequest{
		ID:          "short-timeout",
		Wait:        true,
		WaitTimeout: 20 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out waiting") {
		t.Fatalf("Status err=%v want timeout", err)
	}
	if elapsed >= statusPollInterval/2 {
		t.Fatalf("status wait elapsed=%s, want it bounded by the 20ms wait timeout rather than the 200ms poll interval", elapsed)
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

func TestWarmupRejectsActionsRunnerAndEmitsTiming(t *testing.T) {
	backend := newTestBackend(newTestConfig(), newRunner(nil, nil), io.Discard, io.Discard)
	if err := backend.Warmup(context.Background(), WarmupRequest{ActionsRunner: true}); err == nil || !strings.Contains(err.Error(), "--actions-runner") {
		t.Fatalf("actions runner err=%v", err)
	}

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	runner := newRunner(map[string]scriptedReply{"create": {stdout: ""}}, nil)
	backend = newTestBackend(newTestConfig(), runner, &stdout, &stderr)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: "my-app", Root: t.TempDir()}, TimingJSON: true}); err != nil {
		t.Fatalf("Warmup timing err=%v", err)
	}
	if !strings.Contains(stdout.String(), "warmup complete") || !strings.Contains(stderr.String(), `"provider":"docker-sandbox"`) {
		t.Fatalf("stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
}

func TestDoctorSuccessAndErrorGuidance(t *testing.T) {
	success := newRunner(map[string]scriptedReply{
		"version":  {stdout: "Client Version:  v0.31.3 fake\n"},
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
	if len(okResult.Checks) != 4 || okResult.Checks[1].Check != "sbx_compatibility" || okResult.Checks[1].Status != "ok" || okResult.Checks[1].Details["baseline"] != baselineSBX {
		t.Fatalf("doctor compatibility checks=%#v", okResult.Checks)
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

func TestDoctorWarnsWhenOptionalDiagnoseFailsAndReportsListParse(t *testing.T) {
	runner := newRunner(map[string]scriptedReply{
		"version":  {stdout: "\n sbx version 0.1.0\n"},
		"ls":       {stdout: `[]`},
		"diagnose": {stderr: "diagnose unavailable", exitCode: 1},
	}, nil)
	result, err := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard).Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor optional diagnose err=%v", err)
	}
	if len(result.Checks) != 4 || result.Checks[1].Status != "warn" || result.Checks[1].Check != "sbx_compatibility" || result.Checks[3].Status != "warn" || result.Checks[3].Details["optional"] != "true" {
		t.Fatalf("doctor checks=%#v", result.Checks)
	}

	badList := newRunner(map[string]scriptedReply{
		"version": {stdout: "sbx version 0.1.0\n"},
		"ls":      {stdout: `42`},
	}, nil)
	_, err = newTestBackend(newTestConfig(), badList, io.Discard, io.Discard).Doctor(context.Background(), DoctorRequest{})
	if err == nil || !strings.Contains(err.Error(), "expected array or object") {
		t.Fatalf("bad list err=%v", err)
	}
}

func TestUnsupportedAgentAndTailscaleOptionsRejectClearly(t *testing.T) {
	cfg := newTestConfig()
	cfg.DockerSandbox.Agent = "codex"
	if _, err := (Provider{}).Configure(cfg, Runtime{Exec: newRunner(nil, nil)}); err == nil || !strings.Contains(err.Error(), "v1 supports shell only") {
		t.Fatalf("Configure err=%v, want unsupported agent rejection", err)
	}
	err := rejectRunOptions(Provider{}.Spec(), RunRequest{Repo: Repo{Root: t.TempDir()}, Options: core.LeaseOptions{Tailscale: core.TailscaleConfig{Enabled: true}}})
	if err == nil || !strings.Contains(err.Error(), "Tailscale") {
		t.Fatalf("rejectRunOptions err=%v, want Tailscale rejection", err)
	}
	err = rejectRunOptions(Provider{}.Spec(), RunRequest{Repo: Repo{Root: t.TempDir()}, Options: core.LeaseOptions{SSHUser: "root", SSHPort: "2222", SSHKey: "/tmp/key"}})
	if err != nil {
		t.Fatalf("inherited SSH config should be ignored for delegated sbx provider, got %v", err)
	}
}

func TestRejectRunOptionsAndCreateRepoValidation(t *testing.T) {
	spec := Provider{}.Spec()
	for name, req := range map[string]RunRequest{
		"desktop":   {Repo: Repo{Root: t.TempDir()}, Options: core.LeaseOptions{Desktop: true}},
		"tailscale": {Repo: Repo{Root: t.TempDir()}, Options: core.LeaseOptions{Tailscale: core.TailscaleConfig{Enabled: true}}},
		"no-root":   {},
	} {
		if err := rejectRunOptions(spec, req); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
	if err := validateCreateRepo(newTestConfig(), Repo{}); err == nil || !strings.Contains(err.Error(), "requires a local workspace") {
		t.Fatalf("empty repo err=%v", err)
	}
	cfg := newTestConfig()
	cfg.DockerSandbox.Clone = true
	if err := validateCreateRepo(cfg, Repo{Root: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "--clone requires") {
		t.Fatalf("clone validation err=%v", err)
	}
	worktreeRoot := t.TempDir()
	if err := os.WriteFile(filepathJoin(worktreeRoot, ".git"), []byte("gitdir: ../.git/worktrees/example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateCreateRepo(cfg, Repo{Root: worktreeRoot}); err == nil || !strings.Contains(err.Error(), "--clone requires") {
		t.Fatalf("fake worktree validation err=%v", err)
	}
}

func TestValidateCreateRepoCloneRejectsGitWorktree(t *testing.T) {
	cfg := newTestConfig()
	cfg.DockerSandbox.Clone = true

	parent := t.TempDir()
	repoRoot := filepathJoin(parent, "repo")
	worktreeRoot := filepathJoin(parent, "wt")
	if err := os.Mkdir(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoRoot, "init", "-q")
	runGit(t, repoRoot, "config", "user.email", "alice@example.com")
	runGit(t, repoRoot, "config", "user.name", "Alice")
	if err := os.WriteFile(filepathJoin(repoRoot, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoRoot, "add", "tracked.txt")
	runGit(t, repoRoot, "commit", "-q", "-m", "init")
	runGit(t, repoRoot, "worktree", "add", "-q", worktreeRoot)

	if err := validateCreateRepo(cfg, Repo{Root: repoRoot}); err != nil {
		t.Fatalf("validateCreateRepo rejected main Git checkout: %v", err)
	}
	if err := validateCreateRepo(cfg, Repo{Root: worktreeRoot}); err == nil || !strings.Contains(err.Error(), "linked Git worktrees are not supported") {
		t.Fatalf("validateCreateRepo err=%v, want linked worktree rejection", err)
	}
}

func TestDockerSandboxWorkdirAndNameHelpers(t *testing.T) {
	if got, err := dockerSandboxWorkdir(newTestConfig(), "/tmp/repo/../repo"); err != nil || got != "/tmp/repo" {
		t.Fatalf("workdir from repo got=%q err=%v", got, err)
	}
	cfg := newTestConfig()
	cfg.DockerSandbox.Agent = "  "
	if got := dockerSandboxAgent(cfg); got != defaultAgent {
		t.Fatalf("blank agent got=%q want default", got)
	}
	cfg.DockerSandbox.Agent = " shell-plus "
	if got := dockerSandboxAgent(cfg); got != "shell-plus" {
		t.Fatalf("trimmed agent got=%q", got)
	}
	cfg.DockerSandbox.Workdir = "relative"
	if _, err := dockerSandboxWorkdir(cfg, ""); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative workdir err=%v", err)
	}
	oldRandomBytes := randomBytes
	defer func() { randomBytes = oldRandomBytes }()
	randomBytes = func(b []byte) (int, error) {
		for i := range b {
			b[i] = byte(i + 1)
		}
		return len(b), nil
	}
	name := newSandboxName(Repo{Name: namePrefix + strings.Repeat("a", 100)})
	if len(name) > maxSandboxNameLen || !strings.HasPrefix(name, namePrefix) || !strings.HasSuffix(name, "-010203") || strings.Contains(name, namePrefix+namePrefix) {
		t.Fatalf("sandbox name=%q len=%d", name, len(name))
	}
	exactBase := strings.Repeat("b", maxSandboxNameLen-len(namePrefix)-1-sandboxNameSuffixLen)
	exactName := newSandboxName(Repo{Name: exactBase})
	if !strings.Contains(exactName, namePrefix+exactBase+"-") || len(exactName) != maxSandboxNameLen {
		t.Fatalf("exact sandbox name=%q len=%d", exactName, len(exactName))
	}
	oversizedName := newSandboxName(Repo{Name: exactBase + "c"})
	if strings.Contains(oversizedName, exactBase+"c") || len(oversizedName) != maxSandboxNameLen {
		t.Fatalf("oversized sandbox name=%q len=%d", oversizedName, len(oversizedName))
	}
	randomBytes = func([]byte) (int, error) { return 0, errors.New("entropy unavailable") }
	if got := randomSuffix(); len(got) == 0 || len(got) > sandboxNameSuffixLen {
		t.Fatalf("fallback suffix=%q", got)
	}
}

func TestDockerSandboxSmallHelpers(t *testing.T) {
	record, ok := findRecord([]sandboxRecord{{ID: "id-1", Name: "name-1"}}, "id-1")
	if !ok || record.Name != "name-1" {
		t.Fatalf("find by id record=%#v ok=%t", record, ok)
	}
	record, ok = findRecord([]sandboxRecord{{ID: "id-2", Name: "name-2"}}, "name-2")
	if !ok || record.ID != "id-2" {
		t.Fatalf("find by name record=%#v ok=%t", record, ok)
	}
	if _, ok = findRecord([]sandboxRecord{{ID: "id-3", Name: "name-3"}}, "missing"); ok {
		t.Fatal("findRecord unexpectedly matched missing value")
	}
	if got := timeoutOrDefault(time.Second, time.Minute); got != time.Second {
		t.Fatalf("primary timeout got=%s", got)
	}
	if got := timeoutOrDefault(0, time.Minute); got != time.Minute {
		t.Fatalf("fallback timeout got=%s", got)
	}
	details := map[string]string{"kind": "version"}
	check := doctorCheck("sbx", nil, details)
	if check.Status != "ok" || check.Details["mutation"] != "false" || check.Details["kind"] != "version" {
		t.Fatalf("ok doctor check=%#v", check)
	}
	check = doctorCheck("sbx", errors.New("boom"), nil)
	if check.Status != "error" || check.Message != "boom" || check.Details["mutation"] != "false" {
		t.Fatalf("error doctor check=%#v", check)
	}
	if got := firstNonEmptyLine("\n\t\n second \n third"); got != "second" {
		t.Fatalf("first line got=%q", got)
	}
	if got := firstNonEmptyLine(" \n\t"); got != "" {
		t.Fatalf("blank first line got=%q", got)
	}
	if !sbxVersionMatchesBaseline("Client Version:  v0.31.3 fake") || !sbxVersionMatchesBaseline("sbx version 0.31.3") || sbxVersionMatchesBaseline("sbx version 0.31.4") || sbxVersionMatchesBaseline("") {
		t.Fatal("sbxVersionMatchesBaseline accepted or rejected wrong versions")
	}
}

func TestBuildCommandShellModePreservesShellScript(t *testing.T) {
	got, err := buildCommand([]string{"echo one && echo two"}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sh", "-lc", "echo one && echo two"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command=%#v want %#v", got, want)
	}
}

func TestBuildCommandSingleShellStringStaysRaw(t *testing.T) {
	got, err := buildCommand([]string{"echo one && echo two"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sh", "-lc", "echo one && echo two"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command=%#v want %#v", got, want)
	}
}

func TestBuildCommandLeadingEnvAssignmentQuotesArgv(t *testing.T) {
	got, err := buildCommand([]string{"GREETING=hello world", "printf", "%s\n", "$GREETING"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sh", "-lc", "GREETING='hello world' 'printf' '%s\n' '$GREETING'"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command=%#v want %#v", got, want)
	}
}

func TestSBXErrorFormattingEdges(t *testing.T) {
	var stdout, stderr bytes.Buffer
	stderr.WriteString("plain failure")
	err := sbxError([]string{"ls"}, 1, &stdout, &stderr, errors.New("spawn failed"))
	if err == nil || !strings.Contains(err.Error(), "spawn failed") || strings.Contains(err.Error(), "exited 1") {
		t.Fatalf("runErr formatting err=%v", err)
	}

	stdout.Reset()
	stderr.Reset()
	stderr.WriteString(strings.Repeat("x", 4100) + "tail-marker")
	err = sbxError([]string{"ls"}, 1, &stdout, &stderr, nil)
	if err == nil {
		t.Fatal("expected sbx error")
	}
	if strings.Contains(err.Error(), "tail-marker") || !strings.Contains(err.Error(), strings.Repeat("x", 32)) {
		t.Fatalf("tail truncation err=%v", err)
	}
}

func TestFlagApplicationAndValidation(t *testing.T) {
	cfg := newTestConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterDockerSandboxProviderFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--docker-sandbox-cli", "/opt/sbx",
		"--docker-sandbox-agent", "shell",
		"--docker-sandbox-template", "ubuntu",
		"--docker-sandbox-cpus", "2",
		"--docker-sandbox-memory", "4g",
		"--docker-sandbox-clone",
		"--docker-sandbox-workdir", "/repo",
		"--docker-sandbox-extra-workspace", "/tmp/extra",
		"--docker-sandbox-kit", "example-org/base",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDockerSandboxProviderFlags(&cfg, fs, values); err != nil {
		t.Fatalf("apply flags err=%v", err)
	}
	if cfg.DockerSandbox.CLIPath != "/opt/sbx" || cfg.DockerSandbox.Template != "ubuntu" || cfg.DockerSandbox.CPUs != 2 || cfg.DockerSandbox.Memory != "4g" || !cfg.DockerSandbox.Clone || cfg.DockerSandbox.Workdir != "/repo" {
		t.Fatalf("cfg=%#v", cfg.DockerSandbox)
	}
	if strings.Join(cfg.DockerSandbox.ExtraWorkspaces, ",") != "/tmp/extra" || strings.Join(cfg.DockerSandbox.Kit, ",") != "example-org/base" {
		t.Fatalf("list cfg=%#v", cfg.DockerSandbox)
	}

	mcpCfg := newTestConfig()
	mcpFS := flag.NewFlagSet("docker-sandbox-mcp", flag.ContinueOnError)
	mcpValues := RegisterDockerSandboxProviderFlags(mcpFS, mcpCfg)
	if err := mcpFS.Parse([]string{"--docker-sandbox-mcp", "context7"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDockerSandboxProviderFlags(&mcpCfg, mcpFS, mcpValues); err != nil {
		t.Fatalf("ApplyDockerSandboxProviderFlags mcp err=%v", err)
	}
	if strings.Join(mcpCfg.DockerSandbox.MCP, ",") != "context7" {
		t.Fatalf("ApplyDockerSandboxProviderFlags mcp cfg=%#v", mcpCfg.DockerSandbox)
	}

	for _, flagName := range []string{"class", "type"} {
		t.Run("rejects "+flagName, func(t *testing.T) {
			cfg := newTestConfig()
			fs := flag.NewFlagSet("docker-sandbox-"+flagName, flag.ContinueOnError)
			fs.String(flagName, "", "")
			values := RegisterDockerSandboxProviderFlags(fs, cfg)
			if err := fs.Parse([]string{"--" + flagName, "standard"}); err != nil {
				t.Fatal(err)
			}
			err := ApplyDockerSandboxProviderFlags(&cfg, fs, values)
			if err == nil || !strings.Contains(err.Error(), "--"+flagName+" is not supported") {
				t.Fatalf("ApplyDockerSandboxProviderFlags %s err=%v", flagName, err)
			}
		})
	}

	otherProvider := newTestConfig()
	otherProvider.Provider = "local-container"
	otherFS := flag.NewFlagSet("other", flag.ContinueOnError)
	otherFS.String("class", "", "")
	otherValues := RegisterDockerSandboxProviderFlags(otherFS, otherProvider)
	if err := otherFS.Parse([]string{"--class", "standard"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDockerSandboxProviderFlags(&otherProvider, otherFS, otherValues); err != nil {
		t.Fatalf("non-docker provider class err=%v", err)
	}

	bad := newTestConfig()
	bad.DockerSandbox.CPUs = -1
	if err := validateConfig(bad); err == nil || !strings.Contains(err.Error(), "cpus") {
		t.Fatalf("negative CPU err=%v", err)
	}
	bad = newTestConfig()
	bad.DockerSandbox.CPUs = 2.25
	if err := validateConfig(bad); err == nil || !strings.Contains(err.Error(), "whole number") {
		t.Fatalf("fractional CPU err=%v", err)
	}
	bad = newTestConfig()
	bad.DockerSandbox.CPUs = math.Inf(1)
	if err := validateConfig(bad); err == nil || !strings.Contains(err.Error(), "finite") {
		t.Fatalf("infinite CPU err=%v", err)
	}
	bad = newTestConfig()
	bad.DockerSandbox.Workdir = "/"
	if err := validateConfig(bad); err == nil || !strings.Contains(err.Error(), "too broad") {
		t.Fatalf("root workdir err=%v", err)
	}
	bad = newTestConfig()
	bad.DockerSandbox.MCP = []string{""}
	if err := validateConfig(bad); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("empty list err=%v", err)
	}
}

func TestStopRemovesStaleClaimWhenSandboxIsAlreadyGone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := leasePrefix + "crabbox-my-app-gone"
	if err := claimLeaseForRepoProviderPond(leaseID, "gone", providerName, "", t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	runner := newRunner(map[string]scriptedReply{"rm": {stderr: "sandbox not found", exitCode: 1}}, nil)
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	if err := backend.Stop(context.Background(), StopRequest{ID: "gone"}); err != nil {
		t.Fatalf("Stop stale claim err=%v", err)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID, providerName); err != nil || ok {
		t.Fatalf("stale claim resolved after stop ok=%t err=%v", ok, err)
	}
}

func TestDockerSandboxPorts(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := leasePrefix + "crabbox-my-app-ports"
	if err := claimLeaseForRepoProviderPond(leaseID, "ports", providerName, "", t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	runner := newRunner(map[string]scriptedReply{
		"ports": {stdout: "127.0.0.1:41000->3000/tcp\n"},
	}, nil)
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	out, err := backend.Ports(context.Background(), PortsRequest{ID: "ports", Publish: []string{"3000"}})
	if err != nil {
		t.Fatalf("Ports err=%v", err)
	}
	if out != "127.0.0.1:41000->3000/tcp" {
		t.Fatalf("ports output=%q", out)
	}
	call := findCall(runner, "ports")
	if call == nil || !reflect.DeepEqual(call.Args, []string{"ports", "crabbox-my-app-ports", "--publish", "3000"}) {
		t.Fatalf("ports call=%#v", call)
	}

	runner = newRunner(map[string]scriptedReply{
		"ports": {stdout: "[]\n"},
	}, nil)
	backend = newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	_, err = backend.Ports(context.Background(), PortsRequest{ID: leaseID, JSON: true, Unpublish: []string{"41000:3000"}})
	if err != nil {
		t.Fatalf("Ports json err=%v", err)
	}
	call = findCall(runner, "ports")
	if call == nil || !reflect.DeepEqual(call.Args, []string{"ports", "crabbox-my-app-ports", "--json", "--unpublish", "41000:3000"}) {
		t.Fatalf("ports json call=%#v", call)
	}

	_, err = backend.Ports(context.Background(), PortsRequest{ID: "user-owned"})
	if err == nil || !strings.Contains(err.Error(), "not claimed by Crabbox") {
		t.Fatalf("unclaimed ports err=%v", err)
	}
}

func TestDockerSandboxCopy(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := leasePrefix + "crabbox-my-app-copy"
	if err := claimLeaseForRepoProviderPond(leaseID, "copy", providerName, "", t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	runner := newRunner(map[string]scriptedReply{
		"cp": {stdout: ""},
	}, nil)
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	err := backend.Copy(context.Background(), CopyRequest{ID: "copy", Source: "./coverage.xml", Destination: "SANDBOX:/tmp/coverage.xml", FollowLink: true})
	if err != nil {
		t.Fatalf("Copy err=%v", err)
	}
	call := findCall(runner, "cp")
	if call == nil || !reflect.DeepEqual(call.Args, []string{"cp", "-L", "./coverage.xml", "crabbox-my-app-copy:/tmp/coverage.xml"}) {
		t.Fatalf("copy call=%#v", call)
	}

	runner = newRunner(map[string]scriptedReply{
		"cp": {stdout: ""},
	}, nil)
	backend = newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	err = backend.Copy(context.Background(), CopyRequest{ID: leaseID, Source: "SANDBOX:/tmp/output.log", Destination: "./output.log"})
	if err != nil {
		t.Fatalf("Copy download err=%v", err)
	}
	call = findCall(runner, "cp")
	if call == nil || !reflect.DeepEqual(call.Args, []string{"cp", "crabbox-my-app-copy:/tmp/output.log", "./output.log"}) {
		t.Fatalf("copy download call=%#v", call)
	}

	runner = newRunner(map[string]scriptedReply{
		"cp": {stdout: ""},
	}, nil)
	backend = newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	err = backend.Copy(context.Background(), CopyRequest{ID: leaseID, Source: "C:\\tmp\\output.log", Destination: "SANDBOX:/tmp/output.log"})
	if err != nil {
		t.Fatalf("Copy windows path err=%v", err)
	}
	call = findCall(runner, "cp")
	if call == nil || !reflect.DeepEqual(call.Args, []string{"cp", "C:\\tmp\\output.log", "crabbox-my-app-copy:/tmp/output.log"}) {
		t.Fatalf("copy windows path call=%#v", call)
	}

	err = backend.Copy(context.Background(), CopyRequest{ID: leaseID, Source: "./a", Destination: "./b"})
	if err == nil || !strings.Contains(err.Error(), "requires one side to use SANDBOX:PATH") {
		t.Fatalf("missing sandbox path err=%v", err)
	}
	err = backend.Copy(context.Background(), CopyRequest{ID: leaseID, Source: "SANDBOX:/a", Destination: "SANDBOX:/b"})
	if err == nil || !strings.Contains(err.Error(), "sandbox-to-sandbox") {
		t.Fatalf("double sandbox err=%v", err)
	}
	err = backend.Copy(context.Background(), CopyRequest{ID: leaseID, Source: "OTHER:/a", Destination: "./b"})
	if err == nil || !strings.Contains(err.Error(), "requires one side to use SANDBOX:PATH") {
		t.Fatalf("bad source err=%v", err)
	}
}

func TestConfigureDoctorRejectsInvalidConfig(t *testing.T) {
	cfg := newTestConfig()
	cfg.DockerSandbox.Agent = "codex"
	if _, err := (Provider{}).ConfigureDoctor(cfg, Runtime{Exec: newRunner(nil, nil)}); err == nil || !strings.Contains(err.Error(), "v1 supports shell only") {
		t.Fatalf("ConfigureDoctor err=%v, want invalid config rejection", err)
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
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

func filepathJoin(elem ...string) string {
	return strings.Join(elem, string(os.PathSeparator))
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func TestSBXErrorClassifiesVirtualization(t *testing.T) {
	err := sbxError([]string{"ls", "--json"}, 1, bytes.NewBufferString(""), bytes.NewBufferString("KVM unavailable"), nil)
	if err == nil || !strings.Contains(err.Error(), "virtualization") {
		t.Fatalf("err=%v", err)
	}
}

func TestSBXErrorClassifiesTimeoutAndStreamedErrors(t *testing.T) {
	if err := sbxError([]string{"ls", "--json"}, 1, bytes.NewBufferString("timeout"), bytes.NewBufferString(""), nil); err == nil || !strings.Contains(err.Error(), "control plane") {
		t.Fatalf("timeout err=%v", err)
	}
	runner := newRunner(map[string]scriptedReply{
		"exec": {err: errors.New("broken pipe")},
	}, nil)
	cli, err := newSBXCLI(newTestConfig(), Runtime{Exec: runner})
	if err != nil {
		t.Fatal(err)
	}
	code, err := cli.execStream(context.Background(), "sandbox", "", "", []string{"true"}, io.Discard, io.Discard)
	if code != 0 || err == nil || !strings.Contains(err.Error(), "broken pipe") {
		t.Fatalf("streamed err code=%d err=%v", code, err)
	}
	runner = newRunner(map[string]scriptedReply{
		"exec": {exitCode: 4, err: errors.New("process failed")},
	}, nil)
	cli, err = newSBXCLI(newTestConfig(), Runtime{Exec: runner})
	if err != nil {
		t.Fatal(err)
	}
	code, err = cli.execStream(context.Background(), "sandbox", "", "", []string{"false"}, io.Discard, io.Discard)
	if code != 4 || err != nil {
		t.Fatalf("nonzero streamed result code=%d err=%v", code, err)
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

func TestRunKeepOnFailureMarksSessionKept(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stderr bytes.Buffer
	runner := newRunner(map[string]scriptedReply{
		"create": {stdout: ""},
		"exec":   {exitCode: 7, stderr: "failed\n"},
		"rm":     {stdout: ""},
	}, nil)
	backend := newTestBackend(newTestConfig(), runner, io.Discard, &stderr)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:          Repo{Name: "my-app", Root: t.TempDir()},
		Command:       []string{"false"},
		KeepOnFailure: true,
	})
	var exitErr core.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("err=%v want exit 7", err)
	}
	if result.Session == nil || !result.Session.Kept {
		t.Fatalf("session=%#v, want kept after keep-on-failure", result.Session)
	}
	if got, want := callVerbs(runner), []string{"create", "exec"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("verbs=%v want kept sandbox without rm %v", got, want)
	}
	if !strings.Contains(stderr.String(), "keep-on-failure: kept lease=") {
		t.Fatalf("stderr missing keep-on-failure hint: %s", stderr.String())
	}
}
