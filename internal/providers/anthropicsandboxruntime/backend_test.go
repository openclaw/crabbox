package anthropicsandboxruntime

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecIsDelegatedOneShotAnthropicSandboxRuntime(t *testing.T) {
	provider := Provider{}
	spec := provider.Spec()
	if spec.Name != providerName || spec.Family != providerFamily {
		t.Fatalf("spec identity=%#v", spec)
	}
	if spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec kind/coordinator=%#v", spec)
	}
	if len(spec.Features) != 0 {
		t.Fatalf("features=%v want none", spec.Features)
	}
	if aliases := provider.Aliases(); !reflect.DeepEqual(aliases, []string{"srt"}) {
		t.Fatalf("aliases=%v", aliases)
	}
	targets := []string{}
	for _, target := range spec.Targets {
		targets = append(targets, target.OS)
	}
	if !reflect.DeepEqual(targets, []string{core.TargetLinux, core.TargetMacOS}) {
		t.Fatalf("targets=%v", targets)
	}
}

func TestProviderFlagsApplyAndValidate(t *testing.T) {
	cfg := newTestConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--anthropic-sandbox-runtime-cli", "/opt/srt", "--anthropic-sandbox-runtime-settings", ".crabbox/srt settings.json", "--anthropic-sandbox-runtime-debug"}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.AnthropicSRT.CLIPath != "/opt/srt" || cfg.AnthropicSRT.Settings != ".crabbox/srt settings.json" || !cfg.AnthropicSRT.Debug {
		t.Fatalf("anthropicSandboxRuntime=%#v", cfg.AnthropicSRT)
	}

	bad := newTestConfig()
	bad.AnthropicSRT.CLIPath = " "
	if err := validateConfig(bad); err == nil || !strings.Contains(err.Error(), "cliPath must not be empty") {
		t.Fatalf("validateConfig err=%v", err)
	}
}

func TestConfigureRequiresRuntimeExec(t *testing.T) {
	cfg := newTestConfig()
	if _, err := (Provider{}).Configure(cfg, Runtime{}); err != nil {
		t.Fatalf("Configure should allow Runtime.Exec check to happen at operation time: %v", err)
	}
	backend := newTestBackend(cfg, nil, io.Discard, io.Discard)
	_, err := backend.Run(context.Background(), RunRequest{Repo: Repo{Name: "my-app", Root: t.TempDir()}, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "requires Runtime.Exec") {
		t.Fatalf("Run err=%v", err)
	}
}

func TestRunBuildsSRTCommandAndStreamsOutput(t *testing.T) {
	cfg := newTestConfig()
	cfg.AnthropicSRT.CLIPath = "/opt/srt"
	cfg.AnthropicSRT.Settings = ".crabbox/srt-settings.json"
	cfg.AnthropicSRT.Debug = true
	runner := &recordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if req.Stdout != nil {
			_, _ = io.WriteString(req.Stdout, "ok\n")
		}
		if req.Stderr != nil {
			_, _ = io.WriteString(req.Stderr, "srt debug\n")
		}
		return LocalCommandResult{ExitCode: 0, Stdout: "ok\n", Stderr: "srt debug\n"}, nil
	}}
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(cfg, runner, &stdout, &stderr)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: "/tmp/my-app"},
		Command: []string{"echo", "hello world"},
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if result.Provider != providerName || !result.SyncDelegated || result.ExitCode != 0 || result.Command <= 0 || result.Total <= 0 {
		t.Fatalf("result=%#v", result)
	}
	if stdout.String() != "ok\n" || !strings.Contains(stderr.String(), "sync_delegated=true") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	call := runner.onlyCall(t)
	if call.Name != "/opt/srt" || call.Dir != "/tmp/my-app" {
		t.Fatalf("call name/dir=%q/%q", call.Name, call.Dir)
	}
	wantArgs := []string{"--debug", "--settings", ".crabbox/srt-settings.json", "-c", "'echo' 'hello world'"}
	if !reflect.DeepEqual(call.Args, wantArgs) {
		t.Fatalf("args=%#v want %#v", call.Args, wantArgs)
	}
}

func TestRunForwardsEnvOutsideArgv(t *testing.T) {
	cfg := newTestConfig()
	secret := "secret-token-value"
	t.Setenv("CRABBOX_SECRET_SHOULD_NOT_LEAK", "host-secret")
	runner := &recordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{ExitCode: 0}, nil
	}}
	var stderr bytes.Buffer
	backend := newTestBackend(cfg, runner, io.Discard, &stderr)
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:       Repo{Name: "my-app", Root: t.TempDir()},
		Command:    []string{"printenv", "SECRET_TOKEN"},
		Env:        map[string]string{"SECRET_TOKEN": secret},
		EnvSummary: true,
		Options:    core.LeaseOptions{EnvAllow: []string{"SECRET_TOKEN"}},
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	call := runner.onlyCall(t)
	if strings.Contains(strings.Join(call.Args, " "), secret) {
		t.Fatalf("secret leaked in argv: %v", call.Args)
	}
	if !envContains(call.Env, "SECRET_TOKEN="+secret) {
		t.Fatalf("env did not include selected secret")
	}
	if envHasKey(call.Env, "CRABBOX_SECRET_SHOULD_NOT_LEAK") {
		t.Fatalf("host environment leaked into SRT invocation: %v", call.Env)
	}
	if strings.Contains(stderr.String(), secret) || !strings.Contains(stderr.String(), "SECRET_TOKEN=set") {
		t.Fatalf("stderr env summary=%q", stderr.String())
	}
}

func TestRunReturnsNonZeroExitWithoutPersistentSession(t *testing.T) {
	runner := &recordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if req.Stderr != nil {
			_, _ = io.WriteString(req.Stderr, "boom\n")
		}
		return LocalCommandResult{ExitCode: 7, Stderr: "boom\n"}, errors.New("exit status 7")
	}}
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: t.TempDir()},
		Command: []string{"false"},
	})
	var exitErr core.ExitError
	if !core.AsExitError(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("Run err=%v result=%#v", err, result)
	}
	if result.Session != nil || result.ExitCode != 7 {
		t.Fatalf("result=%#v", result)
	}
}

func TestRunRejectsUnsupportedOneShotOptions(t *testing.T) {
	backend := newTestBackend(newTestConfig(), &recordingRunner{}, io.Discard, io.Discard)
	tests := []struct {
		name string
		req  RunRequest
		want string
	}{
		{name: "lease id", req: RunRequest{ID: "cbx_123"}, want: "persistent lease ids"},
		{name: "keep", req: RunRequest{Keep: true}, want: "persistent lease ids"},
		{name: "desktop", req: RunRequest{Options: core.LeaseOptions{Desktop: true}}, want: "desktop"},
		{name: "tailscale", req: RunRequest{Options: core.LeaseOptions{Tailscale: core.TailscaleConfig{Enabled: true}}}, want: "Tailscale"},
		{name: "sync only", req: RunRequest{SyncOnly: true}, want: "--sync-only is not supported"},
		{name: "capture", req: RunRequest{CaptureStdout: "stdout.txt"}, want: "--capture-stdout is not supported"},
		{name: "fresh pr", req: RunRequest{FreshPR: core.FreshPRSpec{Owner: "example-org", Repo: "my-app", Number: 1}}, want: "--fresh-pr is not supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.req
			req.Repo = Repo{Name: "my-app", Root: t.TempDir()}
			req.Command = []string{"true"}
			_, err := backend.Run(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run err=%v want %q", err, tt.want)
			}
			if len(backend.rt.Exec.(*recordingRunner).calls) != 0 {
				t.Fatalf("runner called for rejected request")
			}
		})
	}
}

func TestDoctorChecksHelpAndTreatsVersionAsInformational(t *testing.T) {
	runner := &recordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "--help":
			return LocalCommandResult{ExitCode: 0, Stdout: "Usage: srt -c <command>\n"}, nil
		case "--version":
			return LocalCommandResult{ExitCode: 1, Stderr: "not available\n"}, errors.New("version failed")
		default:
			t.Fatalf("unexpected args=%v", req.Args)
			return LocalCommandResult{}, nil
		}
	}}
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor err=%v", err)
	}
	if result.Status != "ok" || !strings.Contains(result.Message, "command_surface=ready") {
		t.Fatalf("doctor result=%#v", result)
	}
	if len(result.Checks) != 2 || result.Checks[1].Status != "warn" || result.Checks[1].Details["authoritative"] != "false" {
		t.Fatalf("checks=%#v", result.Checks)
	}
}

func TestDoctorFailsWhenHelpUnavailable(t *testing.T) {
	runner := &recordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{ExitCode: 127, Stderr: "srt not found"}, errors.New("not found")
	}}
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err == nil || result.Status != "error" || !strings.Contains(result.Message, "command_surface=blocked") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestLifecycleIsOneShot(t *testing.T) {
	backend := newTestBackend(newTestConfig(), &recordingRunner{}, io.Discard, io.Discard)
	if err := backend.Warmup(context.Background(), WarmupRequest{}); err == nil || !strings.Contains(err.Error(), "one-shot") {
		t.Fatalf("Warmup err=%v", err)
	}
	if leases, err := backend.List(context.Background(), ListRequest{}); err != nil || len(leases) != 0 {
		t.Fatalf("List leases=%#v err=%v", leases, err)
	}
	if _, err := backend.Status(context.Background(), StatusRequest{}); err == nil || !strings.Contains(err.Error(), "does not support status") {
		t.Fatalf("Status err=%v", err)
	}
	if err := backend.Stop(context.Background(), StopRequest{}); err == nil || !strings.Contains(err.Error(), "does not support stop") {
		t.Fatalf("Stop err=%v", err)
	}
}

func TestBuildCommandText(t *testing.T) {
	tests := []struct {
		name      string
		command   []string
		shellMode bool
		want      string
	}{
		{name: "argv quotes spaces", command: []string{"echo", "hello world"}, want: "'echo' 'hello world'"},
		{name: "shell operator", command: []string{"printf", "ok", "&&", "cat", "file name"}, want: "'printf' 'ok' && 'cat' 'file name'"},
		{name: "leading env", command: []string{"NAME=hello world", "sh", "-c", "echo \"$NAME\""}, want: "NAME='hello world' 'sh' '-c' 'echo \"$NAME\"'"},
		{name: "single shell string", command: []string{"printf ok && echo done"}, want: "printf ok && echo done"},
		{name: "explicit shell mode", command: []string{"printf", "ok", "&&", "echo", "done"}, shellMode: true, want: "printf ok && echo done"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildCommandText(tt.command, tt.shellMode)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("command=%q want %q", got, tt.want)
			}
		})
	}
}

type recordingRunner struct {
	calls []LocalCommandRequest
	fn    func(LocalCommandRequest) (LocalCommandResult, error)
}

func (r *recordingRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if r.fn != nil {
		return r.fn(req)
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

func newTestConfig() Config {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AnthropicSRT.CLIPath = "srt"
	return cfg
}

func newTestBackend(cfg Config, runner *recordingRunner, stdout, stderr io.Writer) *backend {
	rt := Runtime{Stdout: stdout, Stderr: stderr}
	if runner != nil {
		rt.Exec = runner
	}
	return newBackend(Provider{}.Spec(), cfg, rt).(*backend)
}

func envContains(env []string, want string) bool {
	for _, value := range env {
		if value == want {
			return true
		}
	}
	return false
}

func envHasKey(env []string, key string) bool {
	prefix := key + "="
	for _, value := range env {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
