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
	if aliases := provider.Spec().Aliases; !reflect.DeepEqual(aliases, []string{"srt"}) {
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

func TestSRTFlagPresenceAndForeignValues(t *testing.T) {
	for _, providerName := range []string{"anthropic-sandbox-runtime", "srt"} {
		t.Run(providerName, func(t *testing.T) {
			provider, err := core.ProviderFor(providerName)
			if err != nil {
				t.Fatal(err)
			}
			cfg := newTestConfig()
			cfg.Provider = providerName
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			values := provider.RegisterFlags(fs, cfg)
			cfg.AnthropicSRT = core.AnthropicSRTConfig{CLIPath: "/opt/env-srt", Settings: "env.json", Debug: true}
			before := cfg.AnthropicSRT
			if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if cfg.AnthropicSRT != before {
				t.Fatal("unvisited flags restored registration defaults")
			}
			if err := fs.Parse([]string{"--anthropic-sandbox-runtime-cli=", "--anthropic-sandbox-runtime-settings=", "--anthropic-sandbox-runtime-debug=false"}); err != nil {
				t.Fatal(err)
			}
			if err := provider.ApplyFlags(&cfg, fs, values); err == nil || err.Error() != "anthropicSandboxRuntime cliPath must not be empty" {
				t.Fatalf("explicit empty CLI validation=%v", err)
			}
			if cfg.AnthropicSRT != (core.AnthropicSRTConfig{}) {
				t.Fatalf("all visited values must apply before validation: %#v", cfg.AnthropicSRT)
			}
			cfg.AnthropicSRT.CLIPath = "  "
			for _, foreign := range []any{nil, struct{}{}} {
				if err := provider.ApplyFlags(&cfg, fs, foreign); err != nil {
					t.Fatalf("foreign values reached validation: %v", err)
				}
				if cfg.AnthropicSRT.CLIPath != "  " {
					t.Fatal("foreign values copied flags")
				}
			}
			if err := provider.(Provider).ValidateConfig(cfg); err == nil || err.Error() != "anthropicSandboxRuntime cliPath must not be empty" {
				t.Fatalf("selected whitespace validation=%v", err)
			}
		})
	}
}

func TestSRTLauncherDefaultsAndArgumentsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name, cli, settings string
		debug               bool
		wantCLI             string
		wantArgs            []string
	}{
		{name: "empty", wantCLI: "srt", wantArgs: []string{"-c", "echo ok"}},
		{name: "whitespace", cli: " \t", settings: " \t", wantCLI: "srt", wantArgs: []string{"-c", "echo ok"}},
		{name: "custom", cli: " /opt/example-srt ", settings: " config/settings.json ", debug: true, wantCLI: "/opt/example-srt", wantArgs: []string{"--debug", "--settings", "config/settings.json", "-c", "echo ok"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newTestConfig()
			cfg.AnthropicSRT = core.AnthropicSRTConfig{CLIPath: tc.cli, Settings: tc.settings, Debug: tc.debug}
			before := cfg.AnthropicSRT
			runner := &recordingRunner{}
			client, err := newSRTCLI(cfg, core.Runtime{Exec: runner})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.runCommand(context.Background(), "/workspace/example", "echo ok", nil, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			call := runner.onlyCall(t)
			if call.Name != tc.wantCLI || !reflect.DeepEqual(call.Args, tc.wantArgs) || call.Dir != "/workspace/example" {
				t.Fatalf("recorded launcher=%q args=%v dir=%q", call.Name, call.Args, call.Dir)
			}
			if tc.name != "custom" && call.Name != core.BaseConfig().AnthropicSRT.CLIPath {
				t.Fatal("fallback differs from configured default")
			}
			if cfg.AnthropicSRT != before {
				t.Fatal("launcher changed input config")
			}
		})
	}
}

func TestConfigureRequiresRuntimeExec(t *testing.T) {
	cfg := newTestConfig()
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err != nil {
		t.Fatalf("Configure should allow Runtime.Exec check to happen at operation time: %v", err)
	}
	backend := newTestBackend(cfg, nil, io.Discard, io.Discard)
	_, err := backend.Run(context.Background(), core.RunRequest{Repo: core.Repo{Name: "my-app", Root: t.TempDir()}, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "requires Runtime.Exec") {
		t.Fatalf("Run err=%v", err)
	}
}

func TestRunBuildsSRTCommandAndStreamsOutput(t *testing.T) {
	cfg := newTestConfig()
	cfg.AnthropicSRT.CLIPath = "/opt/srt"
	cfg.AnthropicSRT.Settings = ".crabbox/srt-settings.json"
	cfg.AnthropicSRT.Debug = true
	runner := &recordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if req.Stdout != nil {
			_, _ = io.WriteString(req.Stdout, "ok\n")
		}
		if req.Stderr != nil {
			_, _ = io.WriteString(req.Stderr, "srt debug\n")
		}
		return core.LocalCommandResult{ExitCode: 0, Stdout: "ok\n", Stderr: "srt debug\n"}, nil
	}}
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(cfg, runner, &stdout, &stderr)
	result, err := backend.Run(context.Background(), core.RunRequest{
		Repo:    core.Repo{Name: "my-app", Root: "/tmp/my-app"},
		Command: []string{"echo", "hello world"},
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if result.Provider != providerName || !result.SyncDelegated || result.ExitCode != 0 || result.Command <= 0 || result.Total <= 0 {
		t.Fatalf("result=%#v", result)
	}
	if result.Status != core.RunStatusSucceeded || result.ErrorKind != core.RunErrorNone {
		t.Fatalf("status/error=%q/%q", result.Status, result.ErrorKind)
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

func TestRunKeepsLiteralArgumentsInShellTransport(t *testing.T) {
	runner := &recordingRunner{fn: func(core.LocalCommandRequest) (core.LocalCommandResult, error) { return core.LocalCommandResult{}, nil }}
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	_, err := backend.Run(t.Context(), core.RunRequest{
		Repo:    core.Repo{Name: "my-app", Root: t.TempDir()},
		Command: []string{"printf", "%s", "&&"}, CommandLiteralArgs: map[int]bool{2: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	call := runner.onlyCall(t)
	if got := call.Args[len(call.Args)-1]; got != "'printf' '%s' '&&'" {
		t.Fatalf("command=%q", got)
	}
}

func TestRunForwardsEnvOutsideArgv(t *testing.T) {
	cfg := newTestConfig()
	secret := "secret-token-value"
	t.Setenv("CRABBOX_SECRET_SHOULD_NOT_LEAK", "host-secret")
	runner := &recordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		return core.LocalCommandResult{ExitCode: 0}, nil
	}}
	var stderr bytes.Buffer
	backend := newTestBackend(cfg, runner, io.Discard, &stderr)
	_, err := backend.Run(context.Background(), core.RunRequest{
		Repo:       core.Repo{Name: "my-app", Root: t.TempDir()},
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
	for _, tc := range []struct {
		name       string
		code       int
		cause      error
		publicCode int
		status     core.RunStatus
		kind       core.RunErrorKind
	}{
		{"ordinary exit", 7, errors.New("exit status 7"), 7, core.RunStatusFailed, core.RunErrorCommandExit},
		{"exit without runner error", 7, nil, 7, core.RunStatusFailed, core.RunErrorCommandExit},
		{"canceled", 1, context.Canceled, 1, core.RunStatusCanceled, core.RunErrorCanceled},
		{"deadline", 1, context.DeadlineExceeded, 1, core.RunStatusTimedOut, core.RunErrorTimeout},
		{"joined deadline", 7, errors.Join(errors.New("exit status 7"), context.DeadlineExceeded), 7, core.RunStatusTimedOut, core.RunErrorTimeout},
		{"zero-code provider error", 0, errors.New("runner unavailable"), 1, core.RunStatusFailed, core.RunErrorProvider},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				if req.Stderr != nil {
					_, _ = io.WriteString(req.Stderr, "boom\n")
				}
				return core.LocalCommandResult{ExitCode: tc.code, Stderr: "boom\n"}, tc.cause
			}}
			var stderr bytes.Buffer
			backend := newTestBackend(newTestConfig(), runner, io.Discard, &stderr)
			result, err := backend.Run(context.Background(), core.RunRequest{
				Repo:       core.Repo{Name: "my-app", Root: t.TempDir()},
				Command:    []string{"false"},
				TimingJSON: true,
			})
			var exitErr core.ExitError
			if !core.AsExitError(err, &exitErr) || exitErr.Code != tc.publicCode {
				t.Fatalf("Run err=%v result=%#v", err, result)
			}
			if result.Session != nil || result.ExitCode != tc.code {
				t.Fatalf("result=%#v", result)
			}
			if result.Status != tc.status || result.ErrorKind != tc.kind {
				t.Fatalf("status/error=%q/%q", result.Status, result.ErrorKind)
			}
			if !strings.Contains(stderr.String(), `"runStatus":"`+string(tc.status)+`"`) || !strings.Contains(stderr.String(), `"errorKind":"`+string(tc.kind)+`"`) {
				t.Fatalf("stderr = %q, want %s/%s timing", stderr.String(), tc.status, tc.kind)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("runner cause lost: %v", err)
			}
		})
	}
}

func TestSRTErrorPreservesCauseAndDiagnostic(t *testing.T) {
	cause := errors.New("ordinary runner failure")
	for _, tc := range []struct{ name, stdout, stderr, detail string }{
		{"stderr first", "ignored", " detail\n", "detail"},
		{"stdout fallback", " output\n", "\t", "output"},
		{"cause fallback", "", "", cause.Error()},
		{"bounded prefix", "ignored", strings.Repeat("x", 4100), strings.Repeat("x", 4096)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := srtError([]string{"-c", "false"}, core.LocalCommandResult{ExitCode: 7}, tc.stdout, tc.stderr, cause)
			if err.Error() != "srt -c false failed exit=7: "+tc.detail || !errors.Is(err, cause) {
				t.Fatalf("message or cause changed: %v", err)
			}
		})
	}
}

func TestRunRejectsUnsupportedOneShotOptions(t *testing.T) {
	backend := newTestBackend(newTestConfig(), &recordingRunner{}, io.Discard, io.Discard)
	tests := []struct {
		name string
		req  core.RunRequest
		want string
	}{
		{name: "lease id", req: core.RunRequest{ID: "cbx_123"}, want: "persistent lease ids"},
		{name: "keep", req: core.RunRequest{Keep: true}, want: "persistent lease ids"},
		{name: "desktop", req: core.RunRequest{Options: core.LeaseOptions{Desktop: true}}, want: "desktop"},
		{name: "tailscale", req: core.RunRequest{Options: core.LeaseOptions{Tailscale: core.TailscaleConfig{Enabled: true}}}, want: "Tailscale"},
		{name: "sync only", req: core.RunRequest{SyncOnly: true}, want: "--sync-only is not supported"},
		{name: "capture", req: core.RunRequest{CaptureStdout: "stdout.txt"}, want: "--capture-stdout is not supported"},
		{name: "fresh pr", req: core.RunRequest{FreshPR: core.FreshPRSpec{Owner: "example-org", Repo: "my-app", Number: 1}}, want: "--fresh-pr is not supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.req
			req.Repo = core.Repo{Name: "my-app", Root: t.TempDir()}
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
	runner := &recordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "--help":
			return core.LocalCommandResult{ExitCode: 0, Stdout: "Usage: srt -c <command>\n"}, nil
		case "--version":
			return core.LocalCommandResult{ExitCode: 1, Stderr: "not available\n"}, errors.New("version failed")
		default:
			t.Fatalf("unexpected args=%v", req.Args)
			return core.LocalCommandResult{}, nil
		}
	}}
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
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
	runner := &recordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		return core.LocalCommandResult{ExitCode: 127, Stderr: "srt not found"}, errors.New("not found")
	}}
	backend := newTestBackend(newTestConfig(), runner, io.Discard, io.Discard)
	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err == nil || result.Status != "error" || !strings.Contains(result.Message, "command_surface=blocked") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestLifecycleIsOneShot(t *testing.T) {
	backend := newTestBackend(newTestConfig(), &recordingRunner{}, io.Discard, io.Discard)
	if err := backend.Warmup(context.Background(), core.WarmupRequest{}); err == nil || !strings.Contains(err.Error(), "one-shot") {
		t.Fatalf("Warmup err=%v", err)
	}
	if leases, err := backend.List(context.Background(), core.ListRequest{}); err != nil || len(leases) != 0 {
		t.Fatalf("List leases=%#v err=%v", leases, err)
	}
	if _, err := backend.Status(context.Background(), core.StatusRequest{}); err == nil || !strings.Contains(err.Error(), "does not support status") {
		t.Fatalf("Status err=%v", err)
	}
	if err := backend.Stop(context.Background(), core.StopRequest{}); err == nil || !strings.Contains(err.Error(), "does not support stop") {
		t.Fatalf("Stop err=%v", err)
	}
}

type recordingRunner struct {
	calls []core.LocalCommandRequest
	fn    func(core.LocalCommandRequest) (core.LocalCommandResult, error)
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if r.fn != nil {
		return r.fn(req)
	}
	return core.LocalCommandResult{ExitCode: 0}, nil
}

func (r *recordingRunner) onlyCall(t *testing.T) core.LocalCommandRequest {
	t.Helper()
	if len(r.calls) != 1 {
		t.Fatalf("calls=%#v want one", r.calls)
	}
	return r.calls[0]
}

func newTestConfig() core.Config {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AnthropicSRT.CLIPath = "srt"
	return cfg
}

func newTestBackend(cfg core.Config, runner *recordingRunner, stdout, stderr io.Writer) *backend {
	rt := core.Runtime{Stdout: stdout, Stderr: stderr}
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
