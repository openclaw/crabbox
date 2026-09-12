package applemachine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestConcreteFlagInputAttribution(t *testing.T) {
	for _, raw := range []string{"unvisited", "container", ""} {
		t.Run(raw, func(t *testing.T) {
			cfg := core.Config{Provider: "other", AppleContainer: core.AppleContainerConfig{CLIPath: "container"}}
			want := cfg
			fs := flag.NewFlagSet("inputs", flag.ContinueOnError)
			values := (Provider{}).RegisterFlags(fs, cfg)
			if raw != "unvisited" {
				if err := fs.Parse([]string{"--apple-machine-cli", raw}); err != nil {
					t.Fatal(err)
				}
				want.AppleContainer.CLIPath = raw
				core.RecordProviderFlagInputs(&want, true, "apple-container", "apple-machine")
			}
			if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg, want) {
				t.Fatalf("unexpected applied config: %#v", cfg)
			}
			before := cfg
			if err := (Provider{}).ApplyFlags(&cfg, fs, struct{}{}); err != nil || !reflect.DeepEqual(cfg, before) {
				t.Fatalf("foreign flag storage changed input: %v", err)
			}
		})
	}
}

type recordingRunner struct {
	hook      func(core.LocalCommandRequest) (core.LocalCommandResult, error, bool)
	requests  []core.LocalCommandRequest
	responses map[string]core.LocalCommandResult
	errs      map[string]error
	fallback  core.LocalCommandResult
	err       error
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.requests = append(r.requests, req)
	if r.hook != nil {
		if result, err, handled := r.hook(req); handled {
			return result, err
		}
	}
	key := strings.Join(req.Args, "\x00")
	result, ok := r.responses[key]
	if !ok {
		result = r.fallback
	}
	if req.Stdout != nil {
		_, _ = io.WriteString(req.Stdout, result.Stdout)
	}
	if req.Stderr != nil {
		_, _ = io.WriteString(req.Stderr, result.Stderr)
	}
	if err := r.errs[key]; err != nil {
		return result, err
	}
	return result, r.err
}

func testBackend(runner *recordingRunner) *backend {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleContainer.Image = "ubuntu:26.04"
	cfg.AppleContainer.CPUs = 4
	cfg.AppleContainer.Memory = "8G"
	return newBackend(Provider{}.Spec(), cfg, core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*backend)
}

func TestManagedStateNativeAppleMachineHomeMount(t *testing.T) {
	originalGOOS, originalGOARCH := hostGOOS, hostGOARCH
	hostGOOS, hostGOARCH = "darwin", "arm64"
	t.Cleanup(func() { hostGOOS, hostGOARCH = originalGOOS, originalGOARCH })
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state-outside-repo"))
	for _, noSync := range []bool{false, true} {
		runner := &recordingRunner{}
		_, err := testBackend(runner).Run(t.Context(), core.RunRequest{Repo: core.Repo{Root: filepath.Join(home, "repo")}, NoSync: noSync, Command: []string{"true"}})
		if err == nil || !strings.Contains(err.Error(), "apple-machine home mount") || len(runner.requests) != 0 {
			t.Fatalf("noSync=%t err=%v native calls=%v", noSync, err, runner.requests)
		}
	}
	runner := &recordingRunner{}
	if err := testBackend(runner).createMachine(t.Context(), "fixture"); err == nil || len(runner.requests) != 0 {
		t.Fatalf("mount handoff err=%v calls=%v", err, runner.requests)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := validateRepoMount(filepath.Join(home, "repo")); err != nil {
		t.Fatalf("state outside HOME: %v", err)
	}
	t.Setenv("XDG_STATE_HOME", "")
	if err := validateRepoMount(filepath.Join(home, "repo")); err != nil {
		t.Fatalf("unset compatibility: %v", err)
	}
}

func TestAppleMachineConfigFlags(t *testing.T) {
	defaults := core.Config{AppleContainer: core.AppleContainerConfig{CLIPath: "tool", Image: "image-example", User: "user-example", WorkRoot: "/workspace/example", CPUs: 3, Memory: "6g", ExtraRunArgs: []string{"token"}}}
	fs := flag.NewFlagSet("machine", flag.ContinueOnError)
	v := (Provider{}).RegisterFlags(fs, defaults)
	got := map[string]string{}
	fs.VisitAll(func(f *flag.Flag) { got[f.Name] = f.DefValue })
	want := map[string]string{"apple-machine-cli": "tool", "apple-machine-image": "image-example", "apple-machine-cpus": "3", "apple-machine-memory": "6g"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("four-flag surface=%#v", got)
	}
	cfg := defaults
	cfg.Provider = "apple-machine"
	before := cfg
	if err := (Provider{}).ApplyFlags(&cfg, fs, v); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, before) {
		t.Fatal("unvisited machine flags ran defaults")
	}
	if err := fs.Parse([]string{"--apple-machine-cli=~/literal", "--apple-machine-image=first", "--apple-machine-image=image-example", "--apple-machine-cpus=-2", "--apple-machine-memory=8G"}); err != nil {
		t.Fatal(err)
	}
	if err := (Provider{}).ApplyFlags(&cfg, fs, v); err != nil {
		t.Fatal(err)
	}
	wantConfig := defaults.AppleContainer
	wantConfig.CLIPath = "~/literal"
	wantConfig.CPUs = -2
	wantConfig.Memory = "8G"
	if !reflect.DeepEqual(cfg.AppleContainer, wantConfig) || !core.AppleContainerImageExplicit(cfg) || cfg.SSHUser != "" || cfg.WorkRoot != "" || cfg.ServerType != "" || cfg.TargetOS != "" {
		t.Fatal("machine values/marker must not gain container projection")
	}
	f := flag.NewFlagSet("empty", flag.ContinueOnError)
	values := (Provider{}).RegisterFlags(f, cfg)
	if err := f.Parse([]string{"--apple-machine-cli=", "--apple-machine-image=", "--apple-machine-cpus=0", "--apple-machine-memory="}); err != nil {
		t.Fatal(err)
	}
	if err := (Provider{}).ApplyFlags(&cfg, f, values); err != nil {
		t.Fatal(err)
	}
	if cfg.AppleContainer.CLIPath != "" || cfg.AppleContainer.Image != "" || cfg.AppleContainer.CPUs != 0 || cfg.AppleContainer.Memory != "" || cfg.AppleContainer.User != "user-example" || !reflect.DeepEqual(cfg.AppleContainer.ExtraRunArgs, []string{"token"}) {
		t.Fatal("machine empty assignments/no defaults")
	}
	for _, foreign := range []any{nil, struct{}{}} {
		c := core.Config{Provider: "apple-machine"}
		before := c
		if err := (Provider{}).ApplyFlags(&c, flag.NewFlagSet("foreign", flag.ContinueOnError), foreign); err != nil || !reflect.DeepEqual(c, before) {
			t.Fatal("foreign values changed machine config")
		}
	}
}

func TestCreateMachineArgs(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	if err := b.createMachine(t.Context(), "crabbox-test"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(runner.requests[0].Args, " ")
	want := "machine create --name crabbox-test --home-mount rw --cpus 4 --memory 8G ubuntu:26.04"
	if got != want {
		t.Fatalf("args=%q want %q", got, want)
	}
}

func TestInspectMachineDecodesAppleJSON(t *testing.T) {
	args := []string{"machine", "inspect", "crabbox-test"}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		strings.Join(args, "\x00"): {Stdout: `[{"id":"crabbox-test","status":"running","ipAddress":"192.0.2.4","cpus":4,"memory":8589934592}]`},
	}}
	item, err := testBackend(runner).inspectMachine(t.Context(), "crabbox-test")
	if err != nil {
		t.Fatal(err)
	}
	if item.ID != "crabbox-test" || item.Status != "running" || item.CPUs != 4 {
		t.Fatalf("machine=%+v", item)
	}
}

func TestRunUsesHomeMountedRepoAndEnv(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // claims (and their lock files) must stay out of the real state dir
	originalGOOS, originalGOARCH := hostGOOS, hostGOARCH
	hostGOOS, hostGOARCH = "darwin", "arm64"
	t.Cleanup(func() { hostGOOS, hostGOARCH = originalGOOS, originalGOARCH })
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(home, "src", "example")
	leaseID := "cbx_123456789abc"
	slug := "blue-crab"
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	fixture := newMachineFixture(t, runner)
	fixture.claim(t, leaseID, slug, repo)
	var stderr bytes.Buffer
	b := newBackend(Provider{}.Spec(), testBackend(runner).cfg, core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: &stderr}).(*backend)
	result, err := b.Run(t.Context(), RunRequest{
		Repo:       Repo{Root: repo},
		ID:         leaseID,
		Keep:       true,
		TimingJSON: true,
		Command:    []string{"go", "test", "./..."},
		Env:        map[string]string{"CI": "1", "PATH": "/opt/homebrew/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.LeaseID != leaseID || !result.SyncDelegated {
		t.Fatalf("result=%+v", result)
	}
	if result.Session == nil || result.Session.Provider != providerName || result.Session.LeaseID != leaseID || result.Session.Slug != slug || !result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	if result.Session.CleanupCommand != "crabbox stop --provider apple-machine --id "+shellQuote(leaseID) {
		t.Fatalf("cleanup command=%q", result.Session.CleanupCommand)
	}
	req := runner.requests[len(runner.requests)-1]
	got := strings.Join(req.Args, " ")
	if !strings.Contains(got, "machine run --name crabbox-123456789abc --cwd "+repo+" --env-file ") || !strings.HasSuffix(got, " go test ./...") {
		t.Fatalf("args=%q", got)
	}
	if strings.Contains(got, "CI=1") || strings.Contains(got, "/opt/homebrew/bin") {
		t.Fatalf("environment leaked into argv: %q", got)
	}
	if req.Dir != repo {
		t.Fatalf("dir=%q", req.Dir)
	}
	report := decodeLastTimingReport(t, stderr.String())
	if report.RunStatus != "succeeded" || report.ErrorKind != "" {
		t.Fatalf("timing outcome status=%q kind=%q", report.RunStatus, report.ErrorKind)
	}
}

func TestRunTimingJSONClassifiesCommandFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	originalGOOS, originalGOARCH := hostGOOS, hostGOARCH
	hostGOOS, hostGOARCH = "darwin", "arm64"
	t.Cleanup(func() { hostGOOS, hostGOARCH = originalGOOS, originalGOARCH })
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(home, "src", "example")
	leaseID := "cbx_abcdef123456"
	slug := "failed-command"
	var stderr bytes.Buffer
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
		fallback:  core.LocalCommandResult{ExitCode: 9, Stderr: "boom"},
		err:       nativeExitError(t, 9),
	}
	fixture := newMachineFixture(t, runner)
	fixture.claim(t, leaseID, slug, repo)
	b := newBackend(Provider{}.Spec(), testBackend(runner).cfg, core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: &stderr}).(*backend)
	result, err := b.Run(t.Context(), RunRequest{
		Repo:       Repo{Root: repo},
		ID:         leaseID,
		Keep:       true,
		TimingJSON: true,
		Command:    []string{"false"},
	})
	if err == nil || result.ExitCode != 9 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if result.Session == nil || !result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v, want retained failed reused lease", result.Session)
	}
	report := decodeLastTimingReport(t, stderr.String())
	if report.RunStatus != "failed" || report.ErrorKind != "command-exit" {
		t.Fatalf("timing outcome status=%q kind=%q", report.RunStatus, report.ErrorKind)
	}
}

func TestRunDeletesOneShotMachineSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // one-shot Run claims a generated lease id; keep the claim lock out of the real state dir
	originalGOOS, originalGOARCH := hostGOOS, hostGOARCH
	hostGOOS, hostGOARCH = "darwin", "arm64"
	t.Cleanup(func() { hostGOOS, hostGOARCH = originalGOOS, originalGOARCH })
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(home, "src", "one-shot")
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	newMachineFixture(t, runner)
	b := newBackend(Provider{}.Spec(), testBackend(runner).cfg, core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	result, err := b.Run(t.Context(), RunRequest{
		Repo:    Repo{Root: repo},
		Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil || result.Session.Reused || result.Session.Kept || result.Session.CleanupCommand == "" {
		t.Fatalf("session=%#v, want cleaned one-shot handle", result.Session)
	}
	args := []string{}
	for _, req := range runner.requests {
		args = append(args, strings.Join(req.Args, " "))
	}
	if !strings.Contains(strings.Join(args, "\n"), "machine rm crabbox-") {
		t.Fatalf("remove command not recorded: %v", args)
	}
}

type terminalTimingWriter struct {
	bytes.Buffer
	cause error
}

func TestAppleMachineNativeExitFixture(t *testing.T) {
	value := os.Getenv("CRABBOX_APPLE_NATIVE_EXIT")
	if value == "" {
		return
	}
	code, err := strconv.Atoi(value)
	if err != nil || code < 1 || code > 125 {
		t.Fatal("invalid native fixture exit")
	}
	os.Exit(code)
}

func nativeExitError(t *testing.T, code int) error {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestAppleMachineNativeExitFixture$")
	command.Env = []string{"CRABBOX_APPLE_NATIVE_EXIT=" + strconv.Itoa(code), "GORACE=atexit_sleep_ms=0"}
	err := command.Run()
	processErr, ok := err.(*exec.ExitError)
	if !ok || processErr.ExitCode() != code {
		t.Fatalf("native exit fixture=%v, want plain exit %d", err, code)
	}
	return err
}

func (w *terminalTimingWriter) Write(data []byte) (int, error) {
	if len(data) > 0 && data[0] == '{' && w.cause != nil {
		return 0, w.cause
	}
	return w.Buffer.Write(data)
}

func TestRunTerminalOutcomeAndRetention(t *testing.T) {
	originalGOOS, originalGOARCH := hostGOOS, hostGOARCH
	hostGOOS, hostGOARCH = "darwin", "arm64"
	t.Cleanup(func() { hostGOOS, hostGOARCH = originalGOOS, originalGOARCH })
	commandCause := nativeExitError(t, 23)
	for _, tc := range []struct {
		name                                        string
		code                                        int
		cause                                       error
		cleanup, timing, keepFailure, badEnv, reuse bool
		keep                                        bool
	}{
		{name: "success"},
		{name: "cleanup after success", cleanup: true},
		{name: "command and cleanup", code: 23, cause: commandCause, cleanup: true},
		{name: "command cleanup and timing", code: 23, cause: commandCause, cleanup: true, timing: true},
		{name: "transport and cleanup", cause: io.ErrUnexpectedEOF, cleanup: true},
		{name: "nonzero transport and cleanup", code: 23, cause: io.ErrUnexpectedEOF, cleanup: true},
		{name: "wrapped exit and cleanup", code: 23, cause: fmt.Errorf("synthetic hidden wrapper: %w", commandCause), cleanup: true},
		{name: "joined exit transport and timing", code: 23, cause: errors.Join(commandCause, io.ErrUnexpectedEOF), cleanup: true, timing: true},
		{name: "joined exit cancellation", code: 23, cause: errors.Join(commandCause, context.Canceled), cleanup: true},
		{name: "joined exit deadline", code: 23, cause: errors.Join(commandCause, context.DeadlineExceeded), cleanup: true},
		{name: "mismatched exit and cleanup", code: 17, cause: commandCause, cleanup: true},
		{name: "cancellation and cleanup", cause: context.Canceled, cleanup: true},
		{name: "deadline cleanup and timing", cause: context.DeadlineExceeded, cleanup: true, timing: true},
		{name: "kept command and timing", code: 23, cause: commandCause, timing: true, keepFailure: true},
		{name: "success then timing", timing: true, keepFailure: true},
		{name: "early environment failure retained", badEnv: true, keepFailure: true},
		{name: "environment failure and cleanup", badEnv: true, cleanup: true},
		{name: "kept environment failure", badEnv: true, keep: true},
		{name: "reused success", reuse: true},
		{name: "reused command", reuse: true, code: 23, cause: commandCause},
		{name: "reused success then timing", reuse: true, timing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("TMPDIR", t.TempDir())
			repo := filepath.Join(home, "repo")
			if err := os.Mkdir(repo, 0o700); err != nil {
				t.Fatal(err)
			}
			cleanupCause := errors.New("synthetic removal failure")
			writerCause := errors.New("synthetic timing failure")
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
			fixture := newMachineFixture(t, runner)
			creates, commands, removals := 0, 0, 0
			var envPath string
			fixture.before = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				if len(req.Args) > 1 && req.Args[0] == "machine" {
					switch req.Args[1] {
					case "create":
						creates++
					case "rm":
						removals++
						if tc.cleanup {
							return core.LocalCommandResult{ExitCode: 5, Stderr: cleanupCause.Error()}, cleanupCause, true
						}
					case "run":
						// Readiness also uses machine run, but only the workload has cwd.
						if !strings.Contains(strings.Join(req.Args, " "), " --cwd ") {
							break
						}
						commands++
						for i, arg := range req.Args {
							if arg == "--env-file" {
								envPath = req.Args[i+1]
								info, err := os.Stat(envPath)
								if err != nil || info.Mode().Perm() != 0o600 {
									t.Fatalf("private env file: info=%v err=%v", info, err)
								}
							}
						}
						return core.LocalCommandResult{ExitCode: tc.code, Stderr: "synthetic workload diagnostic"}, tc.cause, true
					}
				}
				return core.LocalCommandResult{}, nil, false
			}
			req := RunRequest{Repo: Repo{Root: repo}, Command: []string{"true"}, Env: map[string]string{"FIXTURE": "synthetic"}, TimingJSON: true, Keep: tc.keep, KeepOnFailure: tc.keepFailure, Label: "terminal-fixture"}
			if tc.badEnv {
				req.Env = map[string]string{"FIXTURE": "invalid\nvalue"}
			}
			if tc.reuse {
				req.ID = "cbx_0123456789ab"
				fixture.claim(t, req.ID, "fixture-machine", repo)
			}
			diagnostics := &terminalTimingWriter{}
			if tc.timing {
				diagnostics.cause = writerCause
			}
			b := newBackend(Provider{}.Spec(), testBackend(runner).cfg, core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: diagnostics}).(*backend)
			result, err := b.Run(t.Context(), req)
			wantCode := tc.code
			wantStatus, wantKind := core.RunStatusSucceeded, core.RunErrorNone
			if tc.cause != nil {
				wantCode = 1
				wantStatus, wantKind = core.RunStatusFailed, core.RunErrorProvider
				if tc.cause == commandCause && tc.code == 23 {
					wantCode = 23
					wantKind = core.RunErrorCommandExit
				}
				if errors.Is(tc.cause, context.Canceled) {
					wantStatus, wantKind = core.RunStatusCanceled, core.RunErrorCanceled
				}
				if errors.Is(tc.cause, context.DeadlineExceeded) {
					wantStatus, wantKind = core.RunStatusTimedOut, core.RunErrorTimeout
				}
			}
			if tc.badEnv {
				wantCode, wantStatus, wantKind = 2, core.RunStatusFailed, core.RunErrorProvider
			}
			if wantCode == 0 && (tc.cleanup || tc.timing) {
				wantCode, wantStatus, wantKind = 1, core.RunStatusFailed, core.RunErrorProvider
			}
			classified := core.FinalizeRunResult(result, err)
			if result.ExitCode != wantCode || classified.Status != wantStatus || classified.ErrorKind != wantKind {
				t.Errorf("terminal outcome=%+v err=%v want code=%d status=%s kind=%s", result, err, wantCode, wantStatus, wantKind)
			}
			var public core.ExitError
			if wantCode == 0 {
				if err != nil {
					t.Errorf("successful control failed: %v", err)
				}
			} else if !errors.As(err, &public) || public.Code != wantCode {
				t.Errorf("public failure=%+v err=%v, want code=%d", public, err, wantCode)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Errorf("primary cause lost: %v", err)
			}
			if tc.cause == commandCause && (strings.Contains(public.Message, commandCause.Error()) || strings.Contains(err.Error(), commandCause.Error())) {
				t.Errorf("hidden native cause escaped the existing workload diagnostic boundary: %v", err)
			}
			if strings.Contains(public.Message, "synthetic hidden wrapper") || err != nil && strings.Contains(err.Error(), "synthetic hidden wrapper") {
				t.Errorf("wrapped native cause escaped the diagnostic boundary: %v", err)
			}
			if tc.cleanup && !strings.Contains(public.Message, cleanupCause.Error()) {
				t.Errorf("cleanup diagnostic missing: %q", public.Message)
			}
			if tc.timing && (!errors.Is(err, writerCause) || !strings.Contains(public.Message, writerCause.Error())) {
				t.Errorf("reporting cause or diagnostic lost: %v", err)
			}
			if tc.code == 23 && !strings.Contains(public.Message, "synthetic workload diagnostic") {
				t.Errorf("primary diagnostic missing: %q", public.Message)
			}
			wantRemove := !tc.reuse && !tc.keep && !(tc.keepFailure && (tc.cause != nil || tc.badEnv))
			wantKept := !wantRemove || tc.cleanup
			if result.Session == nil || result.Session.LeaseID != result.LeaseID || result.Session.Reused != tc.reuse || result.Session.Kept != wantKept || result.Session.CleanupCommand == "" {
				t.Errorf("session=%+v want reused=%t kept=%t", result.Session, tc.reuse, wantKept)
			}
			wantCreates, wantCommands, wantRemovals := 1, 1, 0
			if tc.reuse {
				wantCreates = 0
			}
			if tc.badEnv {
				wantCommands = 0
			}
			if wantRemove {
				wantRemovals = 1
			}
			if creates != wantCreates || commands != wantCommands || removals != wantRemovals {
				t.Errorf("create/run/remove=%d/%d/%d want=%d/%d/%d", creates, commands, removals, wantCreates, wantCommands, wantRemovals)
			}
			claims, claimErr := core.ListLeaseClaims()
			wantClaims := 0
			if wantKept {
				wantClaims = 1
			}
			if claimErr != nil || len(claims) != wantClaims || len(fixture.machines) != wantClaims {
				t.Errorf("claims=%d machines=%d want=%d err=%v", len(claims), len(fixture.machines), wantClaims, claimErr)
			}
			if wantKept && len(claims) == 1 && (claims[0].LeaseID != result.LeaseID || claims[0].CloudImmutableID == "") {
				t.Error("retained session lost its bound claim")
			}
			if envPath != "" {
				if _, statErr := os.Stat(envPath); !os.IsNotExist(statErr) {
					t.Errorf("env file retained: %v", statErr)
				}
			}
			if !tc.timing {
				report := decodeLastTimingReport(t, diagnostics.String())
				if report.ExitCode != wantCode || report.RunStatus != wantStatus || report.ErrorKind != wantKind || report.LeaseID != result.LeaseID || !report.SyncSkipped || !report.SyncDelegated || len(report.SyncPhases) != 0 || report.Label != req.Label {
					t.Errorf("timing=%+v disagrees with terminal outcome", report)
				}
			}
		})
	}
}

func TestValidateRepoMountRejectsOutsideHome(t *testing.T) {
	if err := validateRepoMount("/var/tmp/example"); err == nil || !strings.Contains(err.Error(), "requires the repository under") {
		t.Fatalf("err=%v", err)
	}
}

func TestWriteEnvFileRejectsExplicitHostOwnedVariable(t *testing.T) {
	_, _, err := writeEnvFile(map[string]string{"PATH": "/custom/bin"}, []string{"PATH"})
	if err == nil || !strings.Contains(err.Error(), "cannot forward host-owned") {
		t.Fatalf("err=%v", err)
	}
}

func decodeLastTimingReport(t *testing.T, output string) timingReport {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		start := strings.Index(line, "{")
		if start < 0 {
			continue
		}
		var report timingReport
		if err := json.Unmarshal([]byte(line[start:]), &report); err != nil {
			t.Fatalf("timing json: %v\noutput=%s", err, output)
		}
		return report
	}
	t.Fatalf("output does not contain timing JSON: %s", output)
	return timingReport{}
}
