package crownest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestRunUploadsArchiveStreamsLogsAndCleansUp(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	api := &fakeCrownestClient{baseURL: "https://api.crownest.dev"}
	var stdout, stderr bytes.Buffer
	cfg := testConfig()
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt: core.Runtime{
			Stdout: &stdout,
			Stderr: &stderr,
		},
		newClient: func(core.Config, core.Runtime) (client, error) { return api, nil },
	}

	result, err := b.Run(context.Background(), core.RunRequest{
		Repo:               core.Repo{Root: repoRoot, Name: "demo"},
		Command:            []string{"pnpm", "test", "&&"},
		CommandLiteralArgs: map[int]bool{2: true},
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d", result.ExitCode)
	}
	if !strings.Contains(stdout.String(), "ok\n") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.Contains(result.CommandText, "pnpm") || !strings.Contains(result.CommandText, "test") {
		t.Fatalf("command=%q", result.CommandText)
	}
	if api.created.Command != result.CommandText || api.created.Template != "python-node" || api.created.Keep {
		t.Fatalf("create request=%#v", api.created)
	}
	if api.uploadBytes == 0 || api.uploadSize != int64(api.uploadBytes) || api.finalized.UploadID != "upl_123" || !api.started || api.deletedSandboxID != "" {
		t.Fatalf("fake api state upload=%d size=%d finalized=%#v started=%v deleted=%q", api.uploadBytes, api.uploadSize, api.finalized, api.started, api.deletedSandboxID)
	}
	if result.Session == nil || result.Session.Provider != providerName || result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	for _, want := range []string{"--provider crownest", "--crownest-url 'https://api.crownest.dev'", "--crownest-template 'python-node'", "--id "} {
		if !strings.Contains(result.Session.CleanupCommand, want) {
			t.Fatalf("cleanup command=%q, want %q", result.Session.CleanupCommand, want)
		}
	}
	if strings.Contains(stderr.String(), "cn_test") {
		t.Fatalf("stderr leaked secret: %q", stderr.String())
	}
	if claim, err := core.ReadLeaseClaim(result.LeaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim=%#v err=%v, want one-shot local claim removed without sandbox delete", claim, err)
	}
	if result.CommandText != "'pnpm' 'test' '&&'" {
		t.Fatalf("literal intent lost in final payload: %q", result.CommandText)
	}
}

type crownestOutcomeClock struct{ current time.Time }

func (c *crownestOutcomeClock) Now() time.Time        { return c.current }
func (c *crownestOutcomeClock) Sleep(d time.Duration) { c.current = c.current.Add(d) }

type crownestOutcomeWriter struct {
	bytes.Buffer
	report *core.TimingReport
	err    error
	onTime func()
}

func (w *crownestOutcomeWriter) WriteTimingReport(report core.TimingReport) error {
	if w.onTime != nil {
		w.onTime()
	}
	w.report = &report
	return w.err
}

func TestRunFinalizesCrownestOutcomesAfterTerminalActions(t *testing.T) {
	streamFailure := errors.New("stream disconnected")
	deleteFailure := errors.New("delete failed")
	cancelFailure := errors.New("cancel failed")
	writerFailure := errors.New("timing writer failed")
	typedCleanupFailure := &apiError{StatusCode: 503, err: core.Exit(5, "typed cleanup provider failure")}
	for _, tc := range []struct {
		name        string
		terminal    string
		streamErr   error
		cancelLocal bool
		cancelErr   error
		keep        bool
		keepFailure bool
		deleteErr   error
		writerErr   error
		wantCode    int
		wantStatus  core.RunStatus
		wantKind    core.RunErrorKind
		wantKept    bool
	}{
		{name: "one-shot success control", wantStatus: core.RunStatusSucceeded},
		{name: "kept success control", keep: true, wantKept: true, wantStatus: core.RunStatusSucceeded},
		{name: "keep-on-failure success deletes once", keepFailure: true, wantStatus: core.RunStatusSucceeded},
		{name: "cleanup-only failure", keepFailure: true, deleteErr: deleteFailure, wantKept: true, wantCode: 1, wantStatus: core.RunStatusFailed, wantKind: core.RunErrorProvider},
		{name: "cleanup and writer failure", keepFailure: true, deleteErr: deleteFailure, writerErr: writerFailure, wantKept: true, wantCode: 1, wantStatus: core.RunStatusFailed, wantKind: core.RunErrorProvider},
		{name: "typed cleanup failure keeps public code", keepFailure: true, deleteErr: typedCleanupFailure, wantKept: true, wantCode: 5, wantStatus: core.RunStatusFailed, wantKind: core.RunErrorProvider},
		{name: "typed cleanup code survives writer failure", keepFailure: true, deleteErr: typedCleanupFailure, writerErr: writerFailure, wantKept: true, wantCode: 5, wantStatus: core.RunStatusFailed, wantKind: core.RunErrorProvider},
		{name: "command control", terminal: "command", wantCode: 7, wantStatus: core.RunStatusFailed, wantKind: core.RunErrorCommandExit},
		{name: "retained command control", terminal: "command", keepFailure: true, wantKept: true, wantCode: 7, wantStatus: core.RunStatusFailed, wantKind: core.RunErrorCommandExit},
		{name: "command and writer failure", terminal: "command", writerErr: writerFailure, wantCode: 7, wantStatus: core.RunStatusFailed, wantKind: core.RunErrorCommandExit},
		{name: "platform failure", terminal: "platform", wantCode: 5, wantStatus: core.RunStatusFailed, wantKind: core.RunErrorProvider},
		{name: "platform and writer failure", terminal: "platform", writerErr: writerFailure, wantCode: 5, wantStatus: core.RunStatusFailed, wantKind: core.RunErrorProvider},
		{name: "missing exit failure", terminal: "missing", wantCode: 5, wantStatus: core.RunStatusFailed, wantKind: core.RunErrorProvider},
		{name: "stream failure accepted cancellation", streamErr: streamFailure, wantCode: 1, wantStatus: core.RunStatusFailed, wantKind: core.RunErrorProvider},
		{name: "stream and writer failure", streamErr: streamFailure, writerErr: writerFailure, wantCode: 1, wantStatus: core.RunStatusFailed, wantKind: core.RunErrorProvider},
		{name: "transport deadline preserves cause", streamErr: context.DeadlineExceeded, wantCode: 1, wantStatus: core.RunStatusTimedOut, wantKind: core.RunErrorTimeout},
		{name: "local cancellation and writer failure", streamErr: context.Canceled, cancelLocal: true, writerErr: writerFailure, wantCode: 1, wantStatus: core.RunStatusCanceled, wantKind: core.RunErrorCanceled},
		{name: "failed cancellation keeps recovery claim", streamErr: streamFailure, cancelErr: cancelFailure, wantKept: true, wantCode: 1, wantStatus: core.RunStatusFailed, wantKind: core.RunErrorProvider},
		{name: "cancel deadline cannot replace stream failure", streamErr: streamFailure, cancelErr: context.DeadlineExceeded, writerErr: writerFailure, wantKept: true, wantCode: 1, wantStatus: core.RunStatusFailed, wantKind: core.RunErrorProvider},
		{name: "failed cancellation preserves local cancellation", streamErr: context.Canceled, cancelLocal: true, cancelErr: cancelFailure, wantKept: true, wantCode: 1, wantStatus: core.RunStatusCanceled, wantKind: core.RunErrorCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock := &crownestOutcomeClock{current: time.Unix(1700000000, 0)}
			api := &fakeCrownestClient{baseURL: "https://api.crownest.dev", createSandboxID: "sbx_123", latestRun: workspaceRun{ID: "wsr_123", Status: "running", SandboxID: "sbx_123"}}
			deleteCalls, cancelCalls := 0, 0
			api.deleteHook = func(ctx context.Context) error {
				deleteCalls++
				clock.Sleep(3 * time.Second)
				return tc.deleteErr
			}
			api.cancelHook = func(cleanupCtx context.Context) (workspaceRun, error) {
				cancelCalls++
				if cleanupCtx.Err() != nil {
					t.Errorf("cancellation request inherited canceled context: %v", cleanupCtx.Err())
				}
				if _, ok := cleanupCtx.Deadline(); !ok {
					t.Error("cancellation request has no deadline")
				}
				claim, err := core.ReadLeaseClaim(leasePrefix + "sbx_123")
				if err != nil || claim.LeaseID == "" {
					t.Errorf("claim retired before cancellation attempt: claim=%+v err=%v", claim, err)
				}
				clock.Sleep(2 * time.Second)
				// Accepted cancellation need not be terminal; retain the server-owned contract.
				return workspaceRun{ID: "wsr_123", Status: "canceling"}, tc.cancelErr
			}
			api.stream = func() (io.ReadCloser, error) {
				if tc.cancelLocal {
					cancel()
				}
				if tc.streamErr != nil {
					return nil, tc.streamErr
				}
				terminal := workspaceRun{ID: "wsr_123", Status: "succeeded", SandboxID: "sbx_123"}
				code := 0
				terminal.ExitCode = &code
				switch tc.terminal {
				case "command":
					code, terminal.Status, terminal.FailureReason = 7, "failed", "command_exit"
				case "platform":
					terminal.Status, terminal.FailureReason, terminal.FailureClass = "failed", "provisioning", "platform"
				case "missing":
					terminal.Status, terminal.FailureReason, terminal.ExitCode = "canceled", "timeout", nil
				}
				payload, err := json.Marshal(streamEvent{Type: "terminal", Seq: 1, WorkspaceRun: terminal})
				if err != nil {
					t.Fatal(err)
				}
				return io.NopCloser(strings.NewReader("data: " + string(payload) + "\n\n")), nil
			}
			writer := &crownestOutcomeWriter{err: tc.writerErr}
			writer.onTime = func() {
				if tc.streamErr != nil && cancelCalls != 1 {
					t.Errorf("timing emitted before cancellation: calls=%d", cancelCalls)
				}
			}
			b := &backend{spec: Provider{}.Spec(), cfg: testConfig(), rt: core.Runtime{Stdout: io.Discard, Stderr: writer, Clock: clock}, newClient: func(core.Config, core.Runtime) (client, error) { return api, nil }}
			result, err := b.Run(ctx, core.RunRequest{Repo: core.Repo{Root: tempGitRepo(t), Name: "demo"}, Command: []string{"true"}, Keep: tc.keep, KeepOnFailure: tc.keepFailure, TimingJSON: true})
			final := core.FinalizeRunResult(result, err)
			if final.ExitCode != tc.wantCode || final.Status != tc.wantStatus || final.ErrorKind != tc.wantKind {
				t.Errorf("outcome=(%d,%s,%s), want (%d,%s,%s); error=%v", final.ExitCode, final.Status, final.ErrorKind, tc.wantCode, tc.wantStatus, tc.wantKind, err)
			}
			if tc.wantCode == 0 && err != nil {
				t.Errorf("success error=%v", err)
			}
			if tc.wantCode != 0 {
				var public core.ExitError
				if !errors.As(err, &public) || public.Code != tc.wantCode {
					t.Errorf("public error=%v, want code %d", err, tc.wantCode)
				}
				for _, cause := range []error{tc.streamErr, tc.cancelErr, tc.deleteErr, tc.writerErr} {
					if cause != nil && (!errors.Is(err, cause) || !strings.Contains(public.Message, cause.Error())) {
						t.Errorf("cause or public diagnostic lost: cause=%v error=%v public=%q", cause, err, public.Message)
					}
				}
			}
			if result.Session == nil || result.Session.Kept != tc.wantKept {
				t.Errorf("session=%+v, want kept=%v", result.Session, tc.wantKept)
			}
			claim, claimErr := core.ReadLeaseClaim(result.LeaseID)
			if claimErr != nil || (claim.LeaseID != "") != tc.wantKept {
				t.Errorf("claim=%+v err=%v, want retained=%v", claim, claimErr, tc.wantKept)
			}
			wantDeletes := 0
			if tc.keepFailure && tc.terminal == "" && tc.streamErr == nil {
				wantDeletes = 1
			}
			if deleteCalls != wantDeletes {
				t.Errorf("delete calls=%d, want %d", deleteCalls, wantDeletes)
			}
			wantTotal := time.Duration(deleteCalls*3+cancelCalls*2) * time.Second
			if result.Total != wantTotal {
				t.Errorf("total=%s, want terminal actions included: %s", result.Total, wantTotal)
			}
			if writer.report == nil || writer.report.ExitCode != tc.wantCode || writer.report.RunStatus != tc.wantStatus || writer.report.ErrorKind != tc.wantKind || writer.report.TotalMs != wantTotal.Milliseconds() {
				t.Errorf("timing=%+v, want final (%d,%s,%s) total=%s", writer.report, tc.wantCode, tc.wantStatus, tc.wantKind, wantTotal)
			}
		})
	}
}

func TestRunPreservesFirstTypedTimingWriterFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeCrownestClient{baseURL: "https://api.crownest.dev"}
	writerFailure := core.Exit(69, "typed timing writer failure")
	writer := &crownestOutcomeWriter{err: writerFailure}
	b := &backend{spec: Provider{}.Spec(), cfg: testConfig(), rt: core.Runtime{Stdout: io.Discard, Stderr: writer}, newClient: func(core.Config, core.Runtime) (client, error) { return api, nil }}
	result, err := b.Run(context.Background(), core.RunRequest{Repo: core.Repo{Root: tempGitRepo(t), Name: "demo"}, Command: []string{"true"}, TimingJSON: true})
	var public core.ExitError
	if !errors.As(err, &public) || public.Code != 69 || !errors.Is(err, writerFailure) {
		t.Errorf("typed writer failure changed: result=%+v error=%v", result, err)
	}
	if result.ExitCode != 69 || result.Status != core.RunStatusFailed || result.ErrorKind != core.RunErrorProvider {
		t.Errorf("outcome=%+v, want 69/failed/provider-error", result)
	}
	// A rejected record cannot be rewritten after the writer returns its error.
	if writer.report == nil || writer.report.ExitCode != 0 || writer.report.RunStatus != core.RunStatusSucceeded {
		t.Errorf("attempted timing=%+v, want the completed successful outcome", writer.report)
	}
}

func TestRunCleanupCommandIncludesCrownestScopeFlags(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeCrownestClient{baseURL: "https://crownest.internal.example/api"}
	cfg := testConfig()
	cfg.Crownest.APIURL = api.BaseURL()
	cfg.Crownest.ProjectID = "proj_custom"
	cfg.Crownest.Template = "node-22"
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(core.Config, core.Runtime) (client, error) {
			return api, nil
		},
	}

	result, err := b.Run(context.Background(), core.RunRequest{
		Repo:    core.Repo{Root: repoRoot, Name: "demo"},
		Command: []string{"pnpm", "test"},
		Keep:    true,
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	for _, want := range []string{
		"--provider crownest",
		"--crownest-url 'https://crownest.internal.example/api'",
		"--crownest-project-id 'proj_custom'",
		"--crownest-template 'node-22'",
		"--id ",
	} {
		if !strings.Contains(result.Session.CleanupCommand, want) {
			t.Fatalf("cleanup command=%q, want %q", result.Session.CleanupCommand, want)
		}
	}
}

func TestRunMarksSessionKeptWhenRetainedCleanupFails(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeCrownestClient{
		baseURL:   "https://api.crownest.dev",
		deleteErr: errors.New("delete failed"),
	}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  testConfig(),
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(core.Config, core.Runtime) (client, error) {
			return api, nil
		},
	}

	result, err := b.Run(context.Background(), core.RunRequest{
		Repo:          core.Repo{Root: repoRoot, Name: "demo"},
		Command:       []string{"pnpm", "test"},
		KeepOnFailure: true,
	})
	if err == nil || !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("err=%v, want cleanup failure", err)
	}
	if !api.created.Keep {
		t.Fatalf("create request=%#v, want retained sandbox for keep-on-failure cleanup", api.created)
	}
	if result.Session == nil || !result.Session.Kept {
		t.Fatalf("session=%#v, want kept session after cleanup failure", result.Session)
	}
}

func TestRunRejectsWorkspaceEnvUntilCrownestSupportsIt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  testConfig(),
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(core.Config, core.Runtime) (client, error) {
			return &fakeCrownestClient{baseURL: "https://api.crownest.dev"}, nil
		},
	}
	_, err := b.Run(context.Background(), core.RunRequest{
		Repo:    core.Repo{Root: tempGitRepo(t), Name: "demo"},
		Command: []string{"printenv", "FOO"},
		Env:     map[string]string{"FOO": "bar"},
	})
	if err == nil || !strings.Contains(err.Error(), "does not support command environment forwarding") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunDistinguishesFrameworkMetadataFromUnsupportedUserEnv(t *testing.T) {
	for _, userName := range []string{"", "FOO", "CRABBOX_CUSTOM", "CRABBOX_RUN_ID_EXTRA", "CRABBOX_SLUG_PREFIX", "CRABBOX_LEASE_ID_SUFFIX"} {
		t.Run(core.Blank(userName, "framework-only"), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			api := &fakeCrownestClient{baseURL: "https://api.crownest.dev"}
			var stderr bytes.Buffer
			b := &backend{spec: Provider{}.Spec(), cfg: testConfig(), rt: core.Runtime{Stdout: io.Discard, Stderr: &stderr}, newClient: func(core.Config, core.Runtime) (client, error) { return api, nil }}
			env := map[string]string{"CRABBOX_LEASE_ID": "framework-lease", "crabbox_run_id": "framework-run", "CrAbBoX_SlUg": "framework-slug", "CROWNEST_API_KEY": "fixture-auth"}
			if userName != "" {
				env[userName] = "user-value"
			}
			_, err := b.Run(context.Background(), core.RunRequest{Repo: core.Repo{Root: tempGitRepo(t), Name: "demo"}, Command: []string{"true"}, Env: env})
			if userName != "" {
				var public core.ExitError
				if !errors.As(err, &public) || public.Code != 2 || !strings.Contains(err.Error(), "does not support command environment forwarding") || api.created.Command != "" {
					t.Fatalf("unsupported user env admitted: error=%v request=%+v", err, api.created)
				}
				return
			}
			if err != nil || !api.started || api.uploadBytes == 0 {
				t.Fatalf("framework metadata prevented run: error=%v started=%v upload=%d", err, api.started, api.uploadBytes)
			}
			payload, err := json.Marshal(api.created)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range env {
				if bytes.Contains(payload, []byte(value)) || strings.Contains(stderr.String(), value) {
					t.Fatalf("local framework/auth value forwarded or printed: request=%s stderr=%s", payload, stderr.String())
				}
			}
		})
	}
}

func TestRunRejectsSyncOnlyBeforeCreatingWorkspaceRun(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	calledClient := false
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  testConfig(),
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(core.Config, core.Runtime) (client, error) {
			calledClient = true
			return &fakeCrownestClient{baseURL: "https://api.crownest.dev"}, nil
		},
	}

	_, err := b.Run(context.Background(), core.RunRequest{
		Repo:     core.Repo{Root: tempGitRepo(t), Name: "demo"},
		Command:  []string{"echo", "should-not-run"},
		SyncOnly: true,
	})
	if err == nil || !strings.Contains(err.Error(), "--sync-only") {
		t.Fatalf("err=%v, want --sync-only rejection", err)
	}
	if calledClient {
		t.Fatalf("sync-only rejection should not create a Crownest client")
	}
}

func TestRunCancelsWorkspaceRunWhenLocalContextIsCanceled(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	api := &fakeCrownestClient{
		baseURL: "https://api.crownest.dev",
		stream: func() (io.ReadCloser, error) {
			cancel()
			return nil, context.Canceled
		},
	}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  testConfig(),
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(core.Config, core.Runtime) (client, error) {
			return api, nil
		},
	}

	_, err := b.Run(ctx, core.RunRequest{
		Repo:    core.Repo{Root: repoRoot, Name: "demo"},
		Command: []string{"pnpm", "test"},
		Keep:    true,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if api.canceledRunID != "wsr_123" {
		t.Fatalf("canceledRunID=%q", api.canceledRunID)
	}
}

func TestRunCancelsWorkspaceRunWhenStreamFailsBeforeTerminal(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeCrownestClient{
		baseURL:        "https://api.crownest.dev",
		startSandboxID: "sbx_stream_failed",
		latestRun:      workspaceRun{ID: "wsr_123", Status: "running", SandboxID: "sbx_stream_failed"},
		stream: func() (io.ReadCloser, error) {
			return nil, errors.New("event stream disconnected")
		},
	}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  testConfig(),
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(core.Config, core.Runtime) (client, error) {
			return api, nil
		},
	}

	result, err := b.Run(context.Background(), core.RunRequest{
		Repo:          core.Repo{Root: repoRoot, Name: "demo"},
		Command:       []string{"pnpm", "test"},
		KeepOnFailure: true,
	})
	if err == nil || !strings.Contains(err.Error(), "crownest stream failed") {
		t.Fatalf("err=%v, want stream failure", err)
	}
	if api.canceledRunID != "wsr_123" {
		t.Fatalf("canceledRunID=%q, want active run cancellation", api.canceledRunID)
	}
	if api.deletedSandboxID != "" {
		t.Fatalf("deletedSandboxID=%q, want retained sandbox", api.deletedSandboxID)
	}
	if result.Session == nil || !result.Session.Kept {
		t.Fatalf("session=%#v, want kept session after stream failure", result.Session)
	}
}

func TestRunReusesClaimWithoutDeletingSandbox(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	api := &fakeCrownestClient{baseURL: "https://api.crownest.dev", startSandboxID: "sbx_reused"}
	leaseID := leasePrefix + "sbx_reused"
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, "kept", providerName, claimScope(api.BaseURL(), cfg), cfg.Pond, repoRoot, cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(core.Config, core.Runtime) (client, error) {
			return api, nil
		},
	}

	result, err := b.Run(context.Background(), core.RunRequest{
		Repo:    core.Repo{Root: repoRoot, Name: "demo"},
		ID:      "kept",
		Command: []string{"pnpm", "test"},
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if !api.created.Keep || api.created.SandboxID != "sbx_reused" {
		t.Fatalf("create request=%#v, want kept reused sandbox", api.created)
	}
	if api.deletedSandboxID != "" {
		t.Fatalf("deleted reused sandbox %q", api.deletedSandboxID)
	}
	if result.Session == nil || !result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
}

func TestRunReuseHonorsOperationLock(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	api := &fakeCrownestClient{baseURL: "https://api.crownest.dev", startSandboxID: "sbx_reused"}
	leaseID := leasePrefix + "sbx_reused"
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, "kept", providerName, claimScope(api.BaseURL(), cfg), cfg.Pond, repoRoot, cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockCrownestLeaseOperation(context.Background(), leaseID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(core.Config, core.Runtime) (client, error) {
			return api, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = b.Run(ctx, core.RunRequest{
		Repo:    core.Repo{Root: repoRoot, Name: "demo"},
		ID:      "kept",
		Command: []string{"pnpm", "test"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context deadline while waiting for operation lock", err)
	}
	if api.getSandboxCalls != 0 || api.created.Command != "" {
		t.Fatalf("run touched remote before lock: getSandboxCalls=%d created=%#v", api.getSandboxCalls, api.created)
	}
}

func TestStatusWaitPollsUntilSandboxReady(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	api := &fakeCrownestClient{
		baseURL:          "https://api.crownest.dev",
		getSandboxStates: []string{"starting", "running"},
	}
	leaseID := leasePrefix + "sbx_123"
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, "status-wait", providerName, claimScope(api.BaseURL(), cfg), cfg.Pond, repoRoot, cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { core.RemoveLeaseClaim(leaseID) })
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(core.Config, core.Runtime) (client, error) {
			return api, nil
		},
	}

	view, err := b.Status(context.Background(), core.StatusRequest{ID: "status-wait", Wait: true, WaitTimeout: time.Second})
	if err != nil {
		t.Fatalf("Status err=%v", err)
	}
	if !view.Ready || view.State != "running" {
		t.Fatalf("view=%#v, want ready running", view)
	}
	if api.getSandboxCalls < 2 {
		t.Fatalf("getSandboxCalls=%d, want polling", api.getSandboxCalls)
	}
}

func TestStopHonorsOperationLock(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	api := &fakeCrownestClient{baseURL: "https://api.crownest.dev"}
	leaseID := leasePrefix + "sbx_stop_locked"
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, "stop-locked", providerName, claimScope(api.BaseURL(), cfg), cfg.Pond, repoRoot, cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockCrownestLeaseOperation(context.Background(), leaseID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(core.Config, core.Runtime) (client, error) {
			return api, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = b.Stop(ctx, core.StopRequest{ID: "stop-locked"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context deadline while waiting for operation lock", err)
	}
	if api.deletedSandboxID != "" {
		t.Fatalf("stop deleted sandbox before lock: %q", api.deletedSandboxID)
	}
}

func TestCleanupSerializesAndRechecksLeaseActivity(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	cfg.IdleTimeout = time.Minute
	api := &fakeCrownestClient{baseURL: "https://api.crownest.dev"}
	leaseID := leasePrefix + "sbx_cleanup_locked"
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, "cleanup-locked", providerName, claimScope(api.BaseURL(), cfg), cfg.Pond, repoRoot, cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	claim.LastUsedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	claim.IdleTimeoutSeconds = 60
	writeCrownestClaimFixture(t, claim)
	unlock, err := lockCrownestLeaseOperation(context.Background(), leaseID)
	if err != nil {
		t.Fatal(err)
	}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(core.Config, core.Runtime) (client, error) {
			return api, nil
		},
	}

	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- b.Cleanup(context.Background(), core.CleanupRequest{})
	}()
	select {
	case err := <-cleanupDone:
		t.Fatalf("cleanup completed while operation lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	claim.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
	writeCrownestClaimFixture(t, claim)
	unlock()
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	if api.deletedSandboxID != "" {
		t.Fatalf("cleanup deleted refreshed active sandbox: %q", api.deletedSandboxID)
	}
}

func TestCrownestOperationLockHonorsContextCancellation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	unlock, err := lockCrownestLeaseOperation(context.Background(), leasePrefix+"lock-test")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := lockCrownestLeaseOperation(ctx, leasePrefix+"lock-test"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context deadline", err)
	}
}

func TestRunRemovesOneShotClaimAfterArchiveSetupFailure(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeCrownestClient{
		baseURL:         "https://api.crownest.dev",
		createSandboxID: "sbx_created",
		transferErr:     errors.New("transfer failed"),
	}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  testConfig(),
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(core.Config, core.Runtime) (client, error) {
			return api, nil
		},
	}

	_, err := b.Run(context.Background(), core.RunRequest{
		Repo:    core.Repo{Root: repoRoot, Name: "demo"},
		Command: []string{"pnpm", "test"},
	})
	if err == nil || !strings.Contains(err.Error(), "transfer failed") {
		t.Fatalf("err=%v, want transfer failure", err)
	}
	if api.deletedSandboxID != "" {
		t.Fatalf("deletedSandboxID=%q, want Crownest-owned one-shot cleanup", api.deletedSandboxID)
	}
	leaseID := leasePrefix + "sbx_created"
	if claim, err := core.ReadLeaseClaim(leaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim=%#v err=%v, want one-shot setup-failure claim removed", claim, err)
	}
}

func TestRunDeletesPartialCreateSandboxWhenWorkspaceRunIDIsMissing(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeCrownestClient{
		baseURL:         "https://api.crownest.dev",
		createRunResult: workspaceRun{SandboxID: "sbx_partial"},
		createRunErr:    errors.New("crownest create workspace run returned no id"),
	}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  testConfig(),
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(core.Config, core.Runtime) (client, error) {
			return api, nil
		},
	}

	_, err := b.Run(context.Background(), core.RunRequest{
		Repo:    core.Repo{Root: repoRoot, Name: "demo"},
		Command: []string{"pnpm", "test"},
	})
	if err == nil || !strings.Contains(err.Error(), "returned no id") {
		t.Fatalf("err=%v, want missing workspace run id", err)
	}
	if api.deletedSandboxID != "sbx_partial" {
		t.Fatalf("deletedSandboxID=%q, want partial sandbox cleanup", api.deletedSandboxID)
	}
	leaseID := leasePrefix + "sbx_partial"
	if claim, err := core.ReadLeaseClaim(leaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim=%#v err=%v, want no partial-create claim", claim, err)
	}
}

func TestRunKeepOnFailureRetainsCreatedSandboxAfterArchiveSetupFailure(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeCrownestClient{
		baseURL:         "https://api.crownest.dev",
		createSandboxID: "sbx_kept_setup",
		transferErr:     errors.New("transfer failed"),
	}
	var stderr bytes.Buffer
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  testConfig(),
		rt:   core.Runtime{Stdout: io.Discard, Stderr: &stderr},
		newClient: func(core.Config, core.Runtime) (client, error) {
			return api, nil
		},
	}

	result, err := b.Run(context.Background(), core.RunRequest{
		Repo:          core.Repo{Root: repoRoot, Name: "demo"},
		Command:       []string{"pnpm", "test"},
		KeepOnFailure: true,
	})
	if err == nil || !strings.Contains(err.Error(), "transfer failed") {
		t.Fatalf("err=%v, want transfer failure", err)
	}
	if api.deletedSandboxID != "" {
		t.Fatalf("deletedSandboxID=%q, want retained sandbox", api.deletedSandboxID)
	}
	leaseID := leasePrefix + "sbx_kept_setup"
	t.Cleanup(func() { core.RemoveLeaseClaim(leaseID) })
	if claim, err := core.ReadLeaseClaim(leaseID); err != nil || claim.LeaseID != leaseID {
		t.Fatalf("claim=%#v err=%v, want retained claim", claim, err)
	}
	if result.Session == nil || !result.Session.Kept || result.Session.LeaseID != leaseID {
		t.Fatalf("session=%#v, want retained setup-failure session", result.Session)
	}
	if !strings.Contains(stderr.String(), "keep-on-failure: kept lease="+leaseID) {
		t.Fatalf("stderr=%q, want keep-on-failure hint", stderr.String())
	}
}

func TestRunKeepDeletesCreatedSandboxWhenLocalClaimFails(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	api := &fakeCrownestClient{
		baseURL:         "https://api.crownest.dev",
		createSandboxID: "sbx_unclaimable",
	}
	leaseID := leasePrefix + "sbx_unclaimable"
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, "existing", providerName, claimScope(api.BaseURL(), cfg), cfg.Pond, filepath.Join(repoRoot, "other"), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { core.RemoveLeaseClaim(leaseID) })
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(core.Config, core.Runtime) (client, error) {
			return api, nil
		},
	}

	_, err := b.Run(context.Background(), core.RunRequest{
		Repo:    core.Repo{Root: repoRoot, Name: "demo"},
		Command: []string{"pnpm", "test"},
		Keep:    true,
	})
	if err == nil || !strings.Contains(err.Error(), "claimed by repo") {
		t.Fatalf("err=%v, want local claim failure", err)
	}
	if api.deletedSandboxID != "sbx_unclaimable" {
		t.Fatalf("deletedSandboxID=%q, want unclaimed sandbox cleanup", api.deletedSandboxID)
	}
}

func TestRunKeepOnFailureRetainsCreatedSandbox(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeCrownestClient{
		baseURL:        "https://api.crownest.dev",
		startSandboxID: "sbx_failed",
		stream: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(strings.Join([]string{
				`data: {"type":"terminal","seq":1,"workspaceRun":{"id":"wsr_123","status":"failed","failureReason":"command_exit","failureClass":"user_command","sandboxId":"sbx_failed","exitCode":7}}`,
				"",
			}, "\n"))), nil
		},
	}
	var stderr bytes.Buffer
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  testConfig(),
		rt:   core.Runtime{Stdout: io.Discard, Stderr: &stderr},
		newClient: func(core.Config, core.Runtime) (client, error) {
			return api, nil
		},
	}

	result, err := b.Run(context.Background(), core.RunRequest{
		Repo:          core.Repo{Root: repoRoot, Name: "demo"},
		Command:       []string{"false"},
		KeepOnFailure: true,
	})
	var exitErr core.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("err=%v, want exit 7", err)
	}
	if api.deletedSandboxID != "" {
		t.Fatalf("deletedSandboxID=%q, want retained sandbox", api.deletedSandboxID)
	}
	if !api.created.Keep {
		t.Fatalf("create request=%#v, want keepSandbox for keep-on-failure", api.created)
	}
	if result.Session == nil || !result.Session.Kept {
		t.Fatalf("session=%#v, want kept session after failure", result.Session)
	}
	if claim, err := core.ReadLeaseClaim(result.LeaseID); err != nil || claim.LeaseID != result.LeaseID {
		t.Fatalf("claim=%#v err=%v, want retained claim", claim, err)
	}
	if !strings.Contains(stderr.String(), "keep-on-failure") {
		t.Fatalf("stderr=%q, want keep-on-failure hint", stderr.String())
	}
}

func TestRunReturnsErrorForCanceledTerminalWithoutExitCode(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeCrownestClient{
		baseURL:        "https://api.crownest.dev",
		startSandboxID: "sbx_canceled",
		stream: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(strings.Join([]string{
				`data: {"type":"terminal","seq":1,"workspaceRun":{"id":"wsr_123","status":"canceled","failureReason":"timeout","failureClass":"platform","sandboxId":"sbx_canceled"}}`,
				"",
			}, "\n"))), nil
		},
	}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  testConfig(),
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(core.Config, core.Runtime) (client, error) {
			return api, nil
		},
	}

	result, err := b.Run(context.Background(), core.RunRequest{
		Repo:    core.Repo{Root: repoRoot, Name: "demo"},
		Command: []string{"pnpm", "test"},
	})
	if err == nil || !strings.Contains(err.Error(), "status=canceled") {
		t.Fatalf("err=%v, want canceled terminal status error", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("exit=%d, want non-zero", result.ExitCode)
	}
	if api.deletedSandboxID != "" {
		t.Fatalf("deletedSandboxID=%q, want Crownest-owned canceled sandbox cleanup", api.deletedSandboxID)
	}
	if claim, err := core.ReadLeaseClaim(result.LeaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim=%#v err=%v, want one-shot terminal-failure claim removed", claim, err)
	}
}

func TestCreateSandboxCleansUpRemoteWhenLocalClaimFails(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	api := &fakeCrownestClient{
		baseURL:         "https://api.crownest.dev",
		createSandboxID: "sbx_new",
	}
	leaseID := leasePrefix + "sbx_new"
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, "existing", providerName, claimScope(api.BaseURL(), cfg), cfg.Pond, filepath.Join(repoRoot, "other"), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	_, _, _, err := b.createSandbox(context.Background(), api, core.Repo{Root: repoRoot, Name: "demo"}, false, "")
	if err == nil || !strings.Contains(err.Error(), "claimed by repo") {
		t.Fatalf("err=%v, want claim repo conflict", err)
	}
	if api.deletedSandboxID != "sbx_new" {
		t.Fatalf("deletedSandboxID=%q, want remote cleanup", api.deletedSandboxID)
	}
}

type fakeCrownestClient struct {
	baseURL          string
	created          createWorkspaceRunRequest
	createSandboxID  string
	createRunResult  workspaceRun
	createRunErr     error
	finalized        finalizeArchiveRequest
	uploadBytes      int
	uploadSize       int64
	started          bool
	startSandboxID   string
	latestRun        workspaceRun
	getSandboxStates []string
	getSandboxCalls  int
	deletedSandboxID string
	canceledRunID    string
	deleteErr        error
	deleteHook       func(context.Context) error
	cancelHook       func(context.Context) (workspaceRun, error)
	transferErr      error
	stream           func() (io.ReadCloser, error)
}

func (f *fakeCrownestClient) BaseURL() string { return f.baseURL }

func (f *fakeCrownestClient) CreateSandbox(context.Context, createSandboxRequest) (sandbox, error) {
	return sandbox{ID: core.Blank(f.createSandboxID, "sbx_123"), Status: "running"}, nil
}

func (f *fakeCrownestClient) GetSandbox(context.Context, string) (sandbox, error) {
	f.getSandboxCalls++
	if len(f.getSandboxStates) > 0 {
		idx := f.getSandboxCalls - 1
		if idx >= len(f.getSandboxStates) {
			idx = len(f.getSandboxStates) - 1
		}
		return sandbox{ID: "sbx_123", Status: f.getSandboxStates[idx]}, nil
	}
	return sandbox{ID: "sbx_123", Status: "running"}, nil
}

func (f *fakeCrownestClient) DeleteSandbox(ctx context.Context, id string) error {
	f.deletedSandboxID = id
	if f.deleteHook != nil {
		return f.deleteHook(ctx)
	}
	return f.deleteErr
}

func (f *fakeCrownestClient) CreateWorkspaceRun(_ context.Context, req createWorkspaceRunRequest, _ string) (workspaceRun, error) {
	f.created = req
	if f.createRunErr != nil || f.createRunResult.ID != "" || f.createRunResult.SandboxID != "" {
		return f.createRunResult, f.createRunErr
	}
	return workspaceRun{ID: "wsr_123", Status: "awaiting_archive", SandboxID: f.createSandboxID}, nil
}

func (f *fakeCrownestClient) CreateArchiveTransfer(_ context.Context, id string, _ createArchiveTransferRequest, _ string) (archiveTransfer, error) {
	if f.transferErr != nil {
		return archiveTransfer{}, f.transferErr
	}
	return archiveTransfer{ID: "upl_123", Method: "PUT", UploadURL: "/upload", MaxSizeBytes: 1 << 30}, nil
}

func (f *fakeCrownestClient) UploadArchive(_ context.Context, _ archiveTransfer, body io.Reader, size int64) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	f.uploadBytes = len(data)
	f.uploadSize = size
	return nil
}

func (f *fakeCrownestClient) FinalizeArchive(_ context.Context, _ string, req finalizeArchiveRequest, _ string) (workspaceRun, error) {
	f.finalized = req
	return workspaceRun{ID: "wsr_123", Status: "archive_uploaded"}, nil
}

func (f *fakeCrownestClient) StartWorkspaceRun(context.Context, string, string) (workspaceRun, error) {
	f.started = true
	sandboxID := core.Blank(f.startSandboxID, "sbx_123")
	return workspaceRun{ID: "wsr_123", Status: "running", SandboxID: sandboxID}, nil
}

func (f *fakeCrownestClient) CancelWorkspaceRun(ctx context.Context, id string, _ string) (workspaceRun, error) {
	f.canceledRunID = id
	if f.cancelHook != nil {
		return f.cancelHook(ctx)
	}
	return workspaceRun{ID: "wsr_123", Status: "canceled"}, nil
}

func (f *fakeCrownestClient) GetWorkspaceRun(context.Context, string) (workspaceRun, error) {
	if f.latestRun.ID != "" {
		return f.latestRun, nil
	}
	code := 0
	return workspaceRun{ID: "wsr_123", Status: "succeeded", SandboxID: "sbx_123", ExitCode: &code}, nil
}

func (f *fakeCrownestClient) StreamWorkspaceRunEvents(context.Context, string, int64) (io.ReadCloser, error) {
	if f.stream != nil {
		return f.stream()
	}
	return io.NopCloser(strings.NewReader(strings.Join([]string{
		`data: {"type":"stdout","seq":1,"data":"ok\n"}`,
		"",
		`data: {"type":"terminal","seq":2,"workspaceRun":{"id":"wsr_123","status":"succeeded","sandboxId":"sbx_123","exitCode":0}}`,
		"",
	}, "\n"))), nil
}

func (f *fakeCrownestClient) Probe(context.Context) error { return nil }

func writeCrownestClaimFixture(t *testing.T, claim core.LeaseClaim) {
	t.Helper()
	data, err := json.MarshalIndent(claim, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims", claim.LeaseID+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func tempGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"test":"echo ok"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "package.json")
	runGit(t, dir, "commit", "-m", "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
