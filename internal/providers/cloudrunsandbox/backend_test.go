package cloudrunsandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type cloudRunSandboxFixedClock struct{ now time.Time }

func (c cloudRunSandboxFixedClock) Now() time.Time { return c.now }

type cloudRunTerminalClock struct{ now time.Time }

func (c *cloudRunTerminalClock) Now() time.Time { return c.now }

type cloudRunTerminalWriter struct {
	bytes.Buffer
	fail error
}

func (w *cloudRunTerminalWriter) Write(p []byte) (int, error) {
	if w.fail != nil && bytes.HasPrefix(p, []byte(`{"provider":`)) {
		return 0, w.fail
	}
	return w.Buffer.Write(p)
}

func TestCloudRunEffectiveDefaultConsumersRecorded(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	for _, tc := range []struct{ name, cli, workdir, wantCLI, wantWorkdir string }{
		{"zero", "", "", "/usr/local/gcp/bin/sandbox", "/tmp/crabbox"},
		{"whitespace", " \t", " \t", "/usr/local/gcp/bin/sandbox", "/tmp/crabbox"},
		{"custom", " /opt/example-sandbox ", " /tmp/example ", "/opt/example-sandbox", "/tmp/example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := core.Config{CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: tc.cli, Workdir: tc.workdir}}
			before := cfg.CloudRunSandbox
			var calls []core.LocalCommandRequest
			rt := core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: recordingLocalExec{handler: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				calls = append(calls, req)
				return core.LocalCommandResult{}, nil
			}}}
			b := NewBackend(Provider{}.Spec(), cfg, rt).(*backend)
			doctor, err := b.Doctor(context.Background(), core.DoctorRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if len(calls) != 1 || calls[0].Name != tc.wantCLI || strings.Join(calls[0].Args, " ") != "--help" {
				t.Fatalf("recorded doctor calls=%v", calls)
			}
			if doctor.Checks[0].Details["cli"] != tc.wantCLI || doctor.Checks[0].Details["workdir"] != tc.wantWorkdir {
				t.Fatalf("doctor details=%v", doctor.Checks[0].Details)
			}
			workdir, err := cloudRunSandboxWorkdir(cfg)
			if err != nil || workdir != tc.wantWorkdir {
				t.Fatalf("workdir=%q error=%v", workdir, err)
			}
			scope, err := b.claimScope()
			sum := sha256.Sum256([]byte("direct:" + tc.wantCLI))
			if err != nil || scope != "direct:"+hex.EncodeToString(sum[:8]) {
				t.Fatalf("scope=%q error=%v", scope, err)
			}
			wantHint := "crabbox stop --provider cloud-run-sandbox"
			if tc.name == "custom" {
				wantHint += " --cloud-run-sandbox-cli '/opt/example-sandbox'"
			}
			wantHint += " --id 'example'"
			if got := cleanupCommand(cfg, "example"); got != wantHint {
				t.Fatalf("cleanup hint=%q want=%q", got, wantHint)
			}
			if cfg.CloudRunSandbox != before {
				t.Fatal("effective reads changed input config")
			}
			if tc.name != "custom" {
				defaults := core.BaseConfig().CloudRunSandbox
				if tc.wantCLI != defaults.CLIPath || tc.wantWorkdir != defaults.Workdir {
					t.Fatal("effective fallback differs from base defaults")
				}
			}
		})
	}
	cfg := core.Config{CloudRunSandbox: core.CloudRunSandboxConfig{GatewayURL: " https://example.invalid/gateway ", CLIPath: "/opt/custom"}}
	if got := cleanupCommand(cfg, "example"); got != "crabbox stop --provider cloud-run-sandbox --cloud-run-sandbox-gateway-url 'https://example.invalid/gateway' --id 'example'" {
		t.Fatalf("gateway cleanup precedence=%q", got)
	}
}

func TestCloudRunSandboxTerminalOutcome(t *testing.T) {
	plain := cloudRunNativeExit(t, 23)
	for _, tc := range []struct {
		name                                                              string
		code, want                                                        int
		cause                                                             error
		cleanup, writer, setup, native, keepFailure, reuse, opaqueCleanup bool
		kind                                                              core.RunErrorKind
		status                                                            core.RunStatus
	}{
		{name: "success"},
		{name: "native command exit", code: 23, want: 23, native: true, kind: core.RunErrorCommandExit},
		{name: "typed cleanup after success", cleanup: true, want: 9, kind: core.RunErrorTimeout, status: core.RunStatusTimedOut},
		{name: "opaque cleanup after success", cleanup: true, opaqueCleanup: true, want: 1, kind: core.RunErrorProvider},
		{name: "command cleanup writer", code: 23, cleanup: true, writer: true, want: 23, kind: core.RunErrorCommandExit},
		{name: "nonzero transport cleanup", code: 23, cause: io.ErrUnexpectedEOF, cleanup: true, want: 1, kind: core.RunErrorProvider},
		{name: "cancel cleanup", cause: context.Canceled, cleanup: true, want: 130, kind: core.RunErrorCanceled, status: core.RunStatusCanceled},
		{name: "deadline cleanup", cause: context.DeadlineExceeded, cleanup: true, want: 124, kind: core.RunErrorTimeout, status: core.RunStatusTimedOut},
		{name: "setup typed cleanup", setup: true, cleanup: true, want: 6, kind: core.RunErrorProvider},
		{name: "writer after deletion", writer: true, keepFailure: true, want: 1, kind: core.RunErrorProvider},
		{name: "kept command writer", code: 23, keepFailure: true, writer: true, want: 23, kind: core.RunErrorCommandExit},
		{name: "reused success", reuse: true},
		{name: "reused writer", reuse: true, writer: true, want: 1, kind: core.RunErrorProvider},
		{name: "safe cause cleanup", cause: shared.ExitErrorWithCause(7, "safe command diagnostic", errors.New("hidden native detail")), cleanup: true, want: 1, kind: core.RunErrorProvider},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateLeaseHome(t)
			clock := &cloudRunTerminalClock{now: time.Unix(1700000000, 0)}
			writerCause := errors.New("synthetic timing failure")
			cleanupCause := errors.Join(errors.New("hidden cleanup detail"), context.DeadlineExceeded)
			cleanupErr := shared.ExitErrorWithCause(9, "safe cleanup diagnostic", cleanupCause)
			if tc.opaqueCleanup {
				cleanupCause = errors.New("safe cleanup diagnostic")
				cleanupErr = cleanupCause
			}
			setupErr := shared.ExitErrorWithCause(6, "safe setup diagnostic", errors.New("hidden setup detail"))
			writer := &cloudRunTerminalWriter{}
			if tc.writer {
				writer.fail = writerCause
			}
			deletes := 0
			transport := &fakeTransport{mode: "direct", onExec: func(_ string, command string) (int, string, string, error) {
				if strings.HasPrefix(command, "mkdir -p") {
					if tc.setup {
						return 0, "", "", setupErr
					}
					return 0, "", "", nil
				}
				clock.now = clock.now.Add(time.Second)
				if tc.native {
					native := &directTransport{rt: core.Runtime{Exec: recordingLocalExec{handler: func(core.LocalCommandRequest) (core.LocalCommandResult, error) {
						return core.LocalCommandResult{ExitCode: 23}, plain
					}}}}
					code, err := native.Exec(t.Context(), "fixture", command, execOptions{}, io.Discard, io.Discard)
					return code, "", "", err
				}
				return tc.code, "", "", tc.cause
			}, onDestroyOwned: func(id, token string) error {
				if id != token {
					t.Errorf("direct ownership changed: id=%s token=%s", id, token)
				}
				deletes++
				clock.now = clock.now.Add(2 * time.Second)
				if tc.cleanup {
					return cleanupErr
				}
				return nil
			}}
			previous := newTransport
			newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return transport, nil }
			t.Cleanup(func() { newTransport = previous })
			b := NewBackend(Provider{}.Spec(), core.Config{CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"}, IdleTimeout: time.Minute}, core.Runtime{Stdout: io.Discard, Stderr: writer, Clock: clock}).(*backend)
			repo := core.Repo{Root: t.TempDir()}
			req := core.RunRequest{Repo: repo, Command: []string{"fixture-command"}, NoSync: true, TimingJSON: true, KeepOnFailure: tc.keepFailure}
			if tc.reuse {
				id, _, _, _, err := b.createSandbox(t.Context(), transport, repo, false, "fixture")
				if err != nil {
					t.Fatal(err)
				}
				req.ID = id
			}
			result, err := b.Run(t.Context(), req)
			wantStatus := tc.status
			if wantStatus == "" {
				wantStatus = core.RunStatusSucceeded
				if tc.want != 0 {
					wantStatus = core.RunStatusFailed
				}
			}
			var public core.ExitError
			hasPublic := errors.As(err, &public)
			if result.ExitCode != tc.want || result.Status != wantStatus || result.ErrorKind != tc.kind || tc.want != 0 && (!hasPublic || public.Code != tc.want) || tc.want == 0 && err != nil {
				t.Errorf("outcome result=%+v publicCode=%d err=%v", result, public.Code, err)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Error("primary cause lost")
			}
			if tc.setup && !errors.Is(err, setupErr) {
				t.Error("setup cause lost")
			}
			if tc.cleanup && (!errors.Is(err, cleanupCause) || !strings.Contains(public.Message, "safe cleanup diagnostic")) {
				t.Error("cleanup cause/message lost")
			}
			if tc.writer && (!errors.Is(err, writerCause) || !strings.Contains(public.Message, writerCause.Error())) {
				t.Error("writer cause/message lost")
			}
			if strings.Contains(fmt.Sprint(err), "hidden") {
				t.Errorf("hidden cause exposed: %v", err)
			}
			wantDelete := !tc.reuse && !(tc.keepFailure && (tc.code != 0 || tc.cause != nil || tc.setup))
			wantKept := !wantDelete || tc.cleanup
			if deletes != boolInt(wantDelete) || result.Session == nil || result.Session.Kept != wantKept || result.Session.Reused != tc.reuse {
				t.Errorf("deletes=%d session=%+v", deletes, result.Session)
			}
			_, exists, claimErr := core.ReadLeaseClaimWithPresence(result.LeaseID)
			if claimErr != nil || exists != wantKept {
				t.Errorf("claim exists=%v err=%v", exists, claimErr)
			}
			wantTotal := time.Second
			if tc.setup {
				wantTotal = 0
			}
			if wantDelete {
				wantTotal += 2 * time.Second
			}
			if result.Total != wantTotal {
				t.Errorf("total=%s want=%s", result.Total, wantTotal)
			}
			if !tc.writer {
				lines := strings.Split(strings.TrimSpace(writer.String()), "\n")
				var report core.TimingReport
				if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
					t.Fatal(err)
				}
				if report.ExitCode != tc.want || report.RunStatus != wantStatus || report.ErrorKind != tc.kind || report.TotalMs != wantTotal.Milliseconds() {
					t.Errorf("timing=%+v", report)
				}
			}
		})
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestCloudRunSandboxClaimReadFailurePreservesPublicCode(t *testing.T) {
	for _, code := range []int{0, 23} {
		for _, keep := range []bool{false, true} {
			t.Run(fmt.Sprintf("command%d_keep%t", code, keep), func(t *testing.T) {
				isolateLeaseHome(t)
				clock := &cloudRunTerminalClock{now: time.Unix(1700000000, 0)}
				var claimPath string
				deletes := 0
				transport := &fakeTransport{mode: "direct", onExec: func(id, command string) (int, string, string, error) {
					if strings.HasPrefix(command, "mkdir -p") {
						return 0, "", "", nil
					}
					// Model a damaged owned claim while command execution holds its fence.
					// The next real guarded read is clearClaimActivity after execution.
					claimPath = filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims", leasePrefix+id+".json")
					original, err := os.ReadFile(claimPath)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.WriteFile(claimPath, original, 0o600) })
					if err := os.WriteFile(claimPath, []byte("{"), 0o600); err != nil {
						t.Fatal(err)
					}
					clock.now = clock.now.Add(time.Second)
					return code, "", "", nil
				}, onDestroy: func(string) error { deletes++; return nil }}
				previous := newTransport
				newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return transport, nil }
				t.Cleanup(func() { newTransport = previous })
				b := NewBackend(Provider{}.Spec(), core.Config{CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"}, IdleTimeout: time.Minute}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Clock: clock}).(*backend)
				result, err := b.Run(t.Context(), core.RunRequest{Repo: core.Repo{Root: t.TempDir()}, Command: []string{"fixture-command"}, NoSync: true, Keep: keep})
				_, _, readErr := core.ReadLeaseClaimWithPresence(result.LeaseID)
				var readPublic, public core.ExitError
				if !errors.As(readErr, &readPublic) || readPublic.Code != 2 {
					t.Fatalf("real claim read code=%d err=%v", readPublic.Code, readErr)
				}
				want := code
				if want == 0 {
					want = 2
				}
				hasPublic := errors.As(err, &public)
				t.Logf("command=%d keep=%t claimReadCode=%d publicCode=%d resultCode=%d", code, keep, readPublic.Code, public.Code, result.ExitCode)
				if !hasPublic || public.Code != want {
					t.Errorf("public code=%d want=%d err=%v", public.Code, want, err)
				}
				if !strings.Contains(err.Error(), "clear cloud-run-sandbox active claim") || !strings.Contains(err.Error(), "parse claim") {
					t.Errorf("claim error context lost: %v", err)
				}
				if result.Command != time.Second || result.Total != time.Second || result.Session == nil || !result.Session.Kept || result.LeaseID == "" || deletes != 0 {
					t.Errorf("result=%+v deletes=%d", result, deletes)
				}
				if data, readErr := os.ReadFile(claimPath); readErr != nil || string(data) != "{" {
					t.Errorf("claim unexpectedly changed: %q %v", data, readErr)
				}
			})
		}
	}
}

func TestCloudRunSandboxClaimRemovalFailurePreservesPublicCode(t *testing.T) {
	for _, code := range []int{0, 23} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			isolateLeaseHome(t)
			clock := &cloudRunTerminalClock{now: time.Unix(1700000000, 0)}
			var claimPath string
			deletes := 0
			transport := &fakeTransport{mode: "direct", onExec: func(_ string, command string) (int, string, string, error) {
				if strings.HasPrefix(command, "mkdir -p") {
					return 0, "", "", nil
				}
				clock.now = clock.now.Add(time.Second)
				return code, "", "", nil
			}, onDestroy: func(id string) error {
				deletes++
				claimPath = filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims", leasePrefix+id+".json")
				original, err := os.ReadFile(claimPath)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(claimPath); _ = os.WriteFile(claimPath, original, 0o600) })
				// Force the real post-destroy claim removal to fail, without a fake ExitError.
				if err := os.Remove(claimPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(claimPath, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(claimPath, "owned-fixture"), []byte("fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
				return nil
			}}
			previous := newTransport
			newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return transport, nil }
			t.Cleanup(func() { newTransport = previous })
			b := NewBackend(Provider{}.Spec(), core.Config{CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"}, IdleTimeout: time.Minute}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Clock: clock}).(*backend)
			result, err := b.Run(t.Context(), core.RunRequest{Repo: core.Repo{Root: t.TempDir()}, Command: []string{"fixture-command"}, NoSync: true})
			var public core.ExitError
			hasPublic := errors.As(err, &public)
			want := code
			if want == 0 {
				want = 2
			}
			t.Logf("command=%d publicCode=%d resultCode=%d", code, public.Code, result.ExitCode)
			if !hasPublic || public.Code != want {
				t.Errorf("public code=%d want=%d err=%v", public.Code, want, err)
			}
			if !strings.Contains(err.Error(), "remove claim") {
				t.Errorf("real removal error absent: %v", err)
			}
			if result.Command != time.Second || result.Total != time.Second || result.Session == nil || !result.Session.Kept || deletes != 1 {
				t.Errorf("result=%+v deletes=%d", result, deletes)
			}
			if _, err := os.Stat(filepath.Join(claimPath, "owned-fixture")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCloudRunSandboxCreateTimeoutRetainsRecoveryClaim(t *testing.T) {
	isolateLeaseHome(t)
	var sandboxID string
	createErr := context.DeadlineExceeded
	transport := &fakeTransport{
		mode: "remote",
		onCreate: func(id string) error {
			sandboxID = id
			return createErr
		},
	}
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
		IdleTimeout:     time.Minute,
	}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	_, _, _, _, err := b.createSandbox(context.Background(), transport, core.Repo{Root: t.TempDir()}, false, "timeout-recovery")
	if !errors.Is(err, createErr) || !strings.Contains(err.Error(), "recovery claim retained") {
		t.Fatalf("create error=%v, want indeterminate recovery", err)
	}
	if sandboxID == "" {
		t.Fatal("create did not receive a sandbox id")
	}
	claim, readErr := core.ReadLeaseClaim(leasePrefix + sandboxID)
	if readErr != nil {
		t.Fatalf("read recovery claim: %v", readErr)
	}
	if claim.LeaseID != leasePrefix+sandboxID || claim.Provider != providerName || claim.Slug != "timeout-recovery" {
		t.Fatalf("recovery claim=%#v", claim)
	}
	if claim.Labels[claimStateLabel] != "recovery" {
		t.Fatalf("recovery state=%q", claim.Labels[claimStateLabel])
	}
	status, statusErr := b.Status(context.Background(), core.StatusRequest{ID: claim.LeaseID})
	if statusErr != nil || status.Ready || status.State != "recovery" {
		t.Fatalf("recovery status=%#v err=%v", status, statusErr)
	}
	leases, listErr := b.List(context.Background(), core.ListRequest{})
	if listErr != nil || len(leases) != 1 || leases[0].Status != "recovery" {
		t.Fatalf("recovery list=%#v err=%v", leases, listErr)
	}
	previousTransport := newTransport
	newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })
	if _, runErr := b.Run(context.Background(), core.RunRequest{ID: claim.LeaseID, Repo: core.Repo{Root: claim.RepoRoot}, Command: []string{"true"}, NoSync: true}); runErr == nil || !strings.Contains(runErr.Error(), "not ready") {
		t.Fatalf("recovery run error=%v", runErr)
	}
}

func TestCloudRunSandboxStatusProbesProviderLiveness(t *testing.T) {
	isolateLeaseHome(t)
	transport := &fakeTransport{mode: "direct", onProbe: func(string, string) error {
		return errSandboxNotFound
	}}
	previousTransport := newTransport
	newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
		IdleTimeout:     time.Minute,
	}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	const leaseID = leasePrefix + "crabbox-missing"
	if err := claimTestCloudRunSandboxLease(leaseID, "missing", scope, t.TempDir(), time.Minute); err != nil {
		t.Fatal(err)
	}
	status, err := b.Status(context.Background(), core.StatusRequest{ID: leaseID})
	if err != nil || status.Ready || status.State != "missing" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	leases, err := b.List(context.Background(), core.ListRequest{})
	if err != nil || len(leases) != 1 || leases[0].Status != "missing" {
		t.Fatalf("leases=%#v err=%v", leases, err)
	}
}

func TestCloudRunSandboxRunPreservesCancellation(t *testing.T) {
	isolateLeaseHome(t)
	transport := &fakeTransport{
		mode: "direct",
		onExec: func(_ string, command string) (int, string, string, error) {
			if strings.Contains(command, "cancel-me") {
				return 130, "", "", context.Canceled
			}
			return 0, "", "", nil
		},
	}
	previousTransport := newTransport
	newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
		IdleTimeout:     time.Minute,
	}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	_, err := b.Run(context.Background(), core.RunRequest{
		Repo:    core.Repo{Root: t.TempDir()},
		Command: []string{"cancel-me"},
		NoSync:  true,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run err=%v", err)
	}
	var exitErr core.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 130 {
		t.Fatalf("exit err=%#v err=%v", exitErr, err)
	}
}

func TestCloudRunSandboxCreateConflictDropsProvisionalClaim(t *testing.T) {
	isolateLeaseHome(t)
	var sandboxID string
	transport := &fakeTransport{mode: "remote", onCreate: func(id string) error {
		sandboxID = id
		return errSandboxAlreadyExists
	}}
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
		IdleTimeout:     time.Minute,
	}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	_, _, _, _, err := b.createSandbox(context.Background(), transport, core.Repo{Root: t.TempDir()}, false, "conflict")
	if !errors.Is(err, errSandboxAlreadyExists) || !strings.Contains(err.Error(), "without taking ownership") {
		t.Fatalf("create error=%v", err)
	}
	if _, exists, readErr := core.ReadLeaseClaimWithPresence(leasePrefix + sandboxID); readErr != nil || exists {
		t.Fatal("definitive conflict retained a destructive ownership claim")
	}
}

func TestCloudRunSandboxCreateConflictRemovalPrecedesWaitingStop(t *testing.T) {
	isolateLeaseHome(t)
	createStarted := make(chan string, 1)
	allowConflict := make(chan struct{})
	destroyed := false
	transport := &fakeTransport{
		mode: "remote",
		onCreate: func(id string) error {
			createStarted <- id
			<-allowConflict
			return errSandboxAlreadyExists
		},
		onDestroy: func(string) error {
			destroyed = true
			return nil
		},
	}
	previousTransport := newTransport
	newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
		IdleTimeout:     time.Minute,
	}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	createDone := make(chan error, 1)
	go func() {
		_, _, _, _, err := b.createSandbox(context.Background(), transport, core.Repo{Root: t.TempDir()}, false, "conflict-race")
		createDone <- err
	}()
	sandboxID := <-createStarted
	stopDone := make(chan error, 1)
	go func() { stopDone <- b.Stop(context.Background(), core.StopRequest{ID: leasePrefix + sandboxID}) }()
	select {
	case err := <-stopDone:
		t.Fatalf("stop bypassed create claim lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowConflict)
	if err := <-createDone; !errors.Is(err, errSandboxAlreadyExists) {
		t.Fatalf("create err=%v", err)
	}
	if err := <-stopDone; err == nil {
		t.Fatal("waiting stop acquired ownership of pre-existing sandbox")
	}
	if destroyed {
		t.Fatal("waiting stop destroyed a pre-existing sandbox")
	}
}

func TestCloudRunSandboxCleanupSkipsInFlightThenDeletesIdle(t *testing.T) {
	for _, state := range []string{"creating", "running"} {
		t.Run(state, func(t *testing.T) {
			isolateLeaseHome(t)
			// Idle has elapsed on the provider clock; only activity should prevent cleanup.
			now := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
			started := make(chan string, 1)
			allowAction := make(chan struct{})
			release := sync.OnceFunc(func() { close(allowAction) })
			gate := func(id string) {
				started <- id
				<-allowAction
			}
			var owner string
			var destroys [][2]string
			transport := &fakeTransport{
				mode: "remote",
				onCreate: func(id string) error {
					owner = id
					if state == "creating" {
						gate(id)
					}
					return nil
				},
				onExec: func(id, command string) (int, string, string, error) {
					if strings.Contains(command, "active") {
						gate(id)
					}
					return 0, "", "", nil
				},
				onDestroyOwned: func(id, token string) error {
					destroys = append(destroys, [2]string{id, token})
					if id != owner || token != owner {
						return errors.New("destroy did not match remote ownership")
					}
					owner = ""
					return nil
				},
			}
			previousTransport := newTransport
			newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return transport, nil }
			t.Cleanup(func() { newTransport = previousTransport })
			b := NewBackend(Provider{}.Spec(), core.Config{
				CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
				IdleTimeout:     time.Second,
			}, core.Runtime{Clock: cloudRunSandboxFixedClock{now: now}, Stdout: io.Discard, Stderr: io.Discard}).(*backend)
			repo := core.Repo{Root: t.TempDir()}
			action := func() error {
				_, _, _, _, err := b.createSandbox(context.Background(), transport, repo, false, state)
				return err
			}
			if state == "running" {
				leaseID, _, _, _, err := b.createSandbox(context.Background(), transport, repo, false, state)
				if err != nil {
					t.Fatal(err)
				}
				action = func() error {
					result, err := b.Run(context.Background(), core.RunRequest{
						ID: leaseID, Repo: repo, Command: []string{"echo", "active"}, NoSync: true,
					})
					if err == nil && (result.Session == nil || !result.Session.Reused || !result.Session.Kept) {
						return errors.New("run did not retain the reused sandbox")
					}
					return err
				}
			}
			wait := func(done <-chan struct{}, name string) bool {
				t.Helper()
				select {
				case <-done:
					return true
				case <-time.After(5 * time.Second):
					t.Errorf("timed out waiting for %s", name)
					return false
				}
			}
			actionDone := make(chan struct{})
			var cleanupDone chan struct{}
			// Release and join before restoring the transport or isolated claim home,
			// including when an assertion fails while the provider action is gated.
			defer func() {
				release()
				wait(actionDone, "provider action")
				if cleanupDone != nil {
					wait(cleanupDone, "cleanup")
				}
			}()
			var actionErr error
			go func() {
				defer close(actionDone)
				actionErr = action()
			}()
			var sandboxID string
			select {
			case sandboxID = <-started:
			case <-actionDone:
				t.Fatalf("action ended before provider gate: %v", actionErr)
			case <-time.After(5 * time.Second):
				t.Fatal("provider action did not reach gate")
			}
			leaseID := leasePrefix + sandboxID
			claimPath := filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims", leaseID+".json")
			before, err := os.ReadFile(claimPath)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot core.LeaseClaim
			if err := json.Unmarshal(before, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.Revision == "" || snapshot.Labels[claimStateLabel] != state || snapshot.Labels[claimOwnershipLabel] != sandboxID || snapshot.IdleTimeoutSeconds != 1 {
				t.Fatalf("in-flight claim=%#v", snapshot)
			}
			assertPreserved := func() {
				t.Helper()
				after, err := os.ReadFile(claimPath)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("cleanup changed claim bytes/revision: %v", err)
				}
				if len(destroys) != 0 || owner != sandboxID {
					t.Fatalf("cleanup changed remote ownership: owner=%q destroys=%v", owner, destroys)
				}
			}
			var stdout, stderr bytes.Buffer
			cleanup := *b
			cleanup.rt.Stdout, cleanup.rt.Stderr = &stdout, &stderr
			cleanupDone = make(chan struct{})
			var cleanupErr error
			go func() {
				defer close(cleanupDone)
				cleanupErr = cleanup.Cleanup(context.Background(), core.CleanupRequest{})
			}()
			if !wait(cleanupDone, "in-flight cleanup without releasing provider action") {
				t.FailNow()
			}
			if cleanupErr != nil || !strings.Contains(stderr.String(), "skip sandbox="+sandboxID+" lease="+leaseID+" reason=in-flight-"+state+"\n") {
				t.Fatalf("cleanup err=%v stderr=%q", cleanupErr, stderr.String())
			}
			if stdout.String() != providerName+" cleanup removed=0 claims_removed=0 checked=1\n" {
				t.Fatalf("cleanup stdout=%q", stdout.String())
			}
			assertPreserved()
			release()
			if !wait(actionDone, "completed provider action") {
				t.FailNow()
			}
			if actionErr != nil {
				t.Fatal(actionErr)
			}
			completed, err := core.ReadLeaseClaim(leaseID)
			if err != nil || completed.Revision == snapshot.Revision || completed.Labels[claimStateLabel] != "" || completed.Labels[claimActiveUntilLabel] != "" {
				t.Fatalf("completed claim=%#v err=%v", completed, err)
			}
			lastUsed, err := time.Parse(time.RFC3339, completed.LastUsedAt)
			if err != nil {
				t.Fatal(err)
			}
			before, err = os.ReadFile(claimPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, idleExpired := range []bool{false, true} {
				cleanupNow := lastUsed
				if idleExpired {
					cleanupNow = cleanupNow.Add(2 * time.Second)
				}
				cleanup.rt.Clock = cloudRunSandboxFixedClock{now: cleanupNow}
				stdout.Reset()
				stderr.Reset()
				if err := cleanup.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
					t.Fatal(err)
				}
				if !idleExpired {
					if !strings.Contains(stderr.String(), "reason=idle-timeout-remaining\n") {
						t.Fatalf("fresh claim cleanup stderr=%q", stderr.String())
					}
					assertPreserved()
				}
			}
			if !strings.Contains(stdout.String(), "delete sandbox="+sandboxID+" lease="+leaseID+" reason=idle-timeout-expired\n") {
				t.Fatalf("expired cleanup stdout=%q", stdout.String())
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
				t.Fatalf("deleted claim exists=%v err=%v", exists, err)
			}
			if owner != "" || len(destroys) != 1 || destroys[0] != [2]string{sandboxID, snapshot.Labels[claimOwnershipLabel]} {
				t.Fatalf("expired cleanup owner=%q destroys=%v", owner, destroys)
			}
		})
	}
}

func TestCloudRunSandboxCleanupReconcilesOnlyOwnedStaleCreate(t *testing.T) {
	t.Run("owned", func(t *testing.T) {
		isolateLeaseHome(t)
		now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
		b := NewBackend(Provider{}.Spec(), core.Config{
			CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
			IdleTimeout:     time.Hour,
		}, core.Runtime{Clock: cloudRunSandboxFixedClock{now: now}, Stdout: io.Discard, Stderr: io.Discard}).(*backend)
		scope, err := b.claimScope()
		if err != nil {
			t.Fatal(err)
		}
		const sandboxID = "crabbox-stale-owned"
		claim, err := core.ClaimLeaseForRepoProviderScopePondWithLabels(leasePrefix+sandboxID, "stale-owned", providerName, scope, "", t.TempDir(), time.Hour, map[string]string{
			claimStateLabel:       "creating",
			claimActiveUntilLabel: now.Add(-time.Minute).Format(time.RFC3339Nano),
			claimOwnershipLabel:   sandboxID,
		})
		if err != nil {
			t.Fatal(err)
		}
		destroyed := false
		transport := &fakeTransport{mode: "direct", onDestroyOwned: func(id, token string) error {
			if id != sandboxID || token != sandboxID {
				t.Fatalf("destroy id=%q token=%q", id, token)
			}
			destroyed = true
			return nil
		}}
		previousTransport := newTransport
		newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return transport, nil }
		t.Cleanup(func() { newTransport = previousTransport })
		if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
			t.Fatal(err)
		}
		if !destroyed {
			t.Fatal("stale owned create was not destroyed")
		}
		if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
			t.Fatalf("claim exists=%v err=%v", exists, err)
		}
	})

	t.Run("missing-token-fails-closed", func(t *testing.T) {
		isolateLeaseHome(t)
		now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
		b := NewBackend(Provider{}.Spec(), core.Config{
			CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
			IdleTimeout:     time.Hour,
		}, core.Runtime{Clock: cloudRunSandboxFixedClock{now: now}, Stdout: io.Discard, Stderr: io.Discard}).(*backend)
		scope, err := b.claimScope()
		if err != nil {
			t.Fatal(err)
		}
		claim, err := core.ClaimLeaseForRepoProviderScopePondWithLabels(leasePrefix+"crabbox-stale-unknown", "stale-unknown", providerName, scope, "", t.TempDir(), time.Hour, map[string]string{
			claimStateLabel:       "creating",
			claimActiveUntilLabel: now.Add(-time.Minute).Format(time.RFC3339Nano),
		})
		if err != nil {
			t.Fatal(err)
		}
		destroyed := false
		transport := &fakeTransport{mode: "direct", onDestroy: func(string) error {
			destroyed = true
			return nil
		}}
		previousTransport := newTransport
		newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return transport, nil }
		t.Cleanup(func() { newTransport = previousTransport })
		if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err == nil || !strings.Contains(err.Error(), "no ownership token") {
			t.Fatalf("cleanup err=%v", err)
		}
		if destroyed {
			t.Fatal("cleanup destroyed a sandbox without ownership proof")
		}
		if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || !exists {
			t.Fatalf("claim exists=%v err=%v", exists, err)
		}
	})
}

func TestCloudRunSandboxCleanupDryRunNeedsNoTransportCredentials(t *testing.T) {
	isolateLeaseHome(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	var stdout bytes.Buffer
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
		IdleTimeout:     time.Hour,
	}, core.Runtime{Clock: cloudRunSandboxFixedClock{now: now}, Stdout: &stdout, Stderr: io.Discard}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	const sandboxID = "crabbox-dry-run"
	if _, err := core.ClaimLeaseForRepoProviderScopePondWithLabels(leasePrefix+sandboxID, "dry-run", providerName, scope, "", t.TempDir(), time.Hour, map[string]string{
		claimOwnershipLabel: sandboxID,
		claimExpiresAtLabel: now.Add(-time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "would delete sandbox="+sandboxID) {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestCloudRunSandboxCreatePersistsTTL(t *testing.T) {
	isolateLeaseHome(t)
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
		IdleTimeout:     time.Hour,
		TTL:             10 * time.Minute,
	}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	_, _, _, claim, err := b.createSandbox(context.Background(), &fakeTransport{mode: "direct"}, core.Repo{Root: t.TempDir()}, false, "ttl")
	if err != nil {
		t.Fatal(err)
	}
	expires, err := time.Parse(time.RFC3339Nano, claim.Labels[claimExpiresAtLabel])
	if err != nil {
		t.Fatalf("ttl label=%q err=%v", claim.Labels[claimExpiresAtLabel], err)
	}
	if due, reason := claimCleanupDue(claim, expires.Add(-time.Second)); due || reason != "idle-timeout-remaining" {
		t.Fatalf("before ttl due=%v reason=%s", due, reason)
	}
	if due, reason := claimCleanupDue(claim, expires); !due || reason != "ttl-expired" {
		t.Fatalf("at ttl due=%v reason=%s", due, reason)
	}
}

func TestRunMissingCommandHasNoSideEffects(t *testing.T) {
	transportCalls := 0
	previousTransport := newTransport
	newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) {
		transportCalls++
		return &fakeTransport{mode: "direct"}, nil
	}
	t.Cleanup(func() { newTransport = previousTransport })
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
	}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	_, err := b.Run(context.Background(), core.RunRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "missing command") {
		t.Fatalf("run error=%v", err)
	}
	if transportCalls != 0 {
		t.Fatalf("transport created %d times", transportCalls)
	}
}

func TestCloudRunSandboxCleanupSkipsClaimReclaimedAfterSnapshot(t *testing.T) {
	isolateLeaseHome(t)
	const sandboxID = "crabbox-reclaimed-123456"
	leaseID := leasePrefix + sandboxID
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
		IdleTimeout:     time.Minute,
	}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	if err := claimTestCloudRunSandboxLease(leaseID, "reclaimed", scope, t.TempDir(), time.Minute); err != nil {
		t.Fatal(err)
	}
	snapshot, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	newRepo := t.TempDir()
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, "reclaimed", providerName, scope, "", newRepo, time.Minute, true); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	destroyed := false
	transport := &fakeTransport{mode: "remote", onDestroy: func(string) error {
		destroyed = true
		return nil
	}}

	removed, err := b.destroyClaimedSandboxIfUnchanged(context.Background(), transport, sandboxID, snapshot)
	if err != nil {
		t.Fatalf("cleanup stale candidate: %v", err)
	}
	if removed {
		t.Fatal("cleanup reported a stale candidate removed")
	}
	if destroyed {
		t.Fatal("cleanup destroyed a sandbox reclaimed after its snapshot")
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil || claim.RepoRoot != newRepo {
		t.Fatalf("reclaimed claim not preserved: claim=%#v err=%v", claim, err)
	}
}

func TestCloudRunSandboxCleanupDestroyFailureRetainsClaim(t *testing.T) {
	isolateLeaseHome(t)
	const sandboxID = "crabbox-destroy-failure-123456"
	leaseID := leasePrefix + sandboxID
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
		IdleTimeout:     time.Minute,
	}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	if err := claimTestCloudRunSandboxLease(leaseID, "destroy-failure", scope, t.TempDir(), time.Minute); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	destroyErr := errors.New("gateway unavailable")
	transport := &fakeTransport{mode: "remote", onDestroy: func(string) error { return destroyErr }}

	removed, err := b.destroyClaimedSandboxIfUnchanged(context.Background(), transport, sandboxID, claim)
	if !errors.Is(err, destroyErr) {
		t.Fatalf("cleanup destroy failure=%v, want %v", err, destroyErr)
	}
	if removed {
		t.Fatal("cleanup reported a failed destroy removed")
	}
	retained, err := core.ReadLeaseClaim(leaseID)
	if err != nil || retained.LeaseID != leaseID {
		t.Fatalf("failed destroy lost claim: claim=%#v err=%v", retained, err)
	}
}

func TestCloudRunSandboxCleanupContinuesAfterDestroyFailure(t *testing.T) {
	isolateLeaseHome(t)
	now := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	var destroyed []string
	destroyErr := errors.New("first destroy failed")
	transport := &fakeTransport{mode: "remote", onDestroy: func(id string) error {
		destroyed = append(destroyed, id)
		if strings.Contains(id, "a-fail") {
			return destroyErr
		}
		return nil
	}}
	previousTransport := newTransport
	newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
		IdleTimeout:     time.Second,
	}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Clock: cloudRunSandboxFixedClock{now: now}}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"crabbox-a-fail", "crabbox-z-success"} {
		if err := claimTestCloudRunSandboxLease(leasePrefix+id, id, scope, t.TempDir(), time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); !errors.Is(err, destroyErr) {
		t.Fatalf("cleanup err=%v, want %v", err, destroyErr)
	}
	if len(destroyed) != 2 {
		t.Fatalf("destroyed=%v", destroyed)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leasePrefix + "crabbox-a-fail"); err != nil || !exists {
		t.Fatalf("failed claim exists=%v err=%v", exists, err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leasePrefix + "crabbox-z-success"); err != nil || exists {
		t.Fatalf("successful claim exists=%v err=%v", exists, err)
	}
}

func TestCloudRunSandboxRunPropagatesAutomaticTeardownFailure(t *testing.T) {
	isolateLeaseHome(t)
	destroyErr := errors.New("delete unavailable")
	transport := &fakeTransport{
		mode:      "remote",
		onExec:    func(string, string) (int, string, string, error) { return 0, "", "", nil },
		onDestroy: func(string) error { return destroyErr },
	}
	previousTransport := newTransport
	newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })
	var stderr bytes.Buffer
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
		IdleTimeout:     time.Minute,
	}, core.Runtime{Stdout: io.Discard, Stderr: &stderr}).(*backend)
	result, err := b.Run(context.Background(), core.RunRequest{
		Repo:       core.Repo{Root: t.TempDir()},
		Command:    []string{"true"},
		NoSync:     true,
		TimingJSON: true,
	})
	if !errors.Is(err, destroyErr) || result.Session == nil || !result.Session.Kept {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, exists, readErr := core.ReadLeaseClaimWithPresence(result.LeaseID); readErr != nil || !exists {
		t.Fatalf("recovery claim exists=%v err=%v", exists, readErr)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	var report map[string]any
	if jsonErr := json.Unmarshal([]byte(lines[len(lines)-1]), &report); jsonErr != nil {
		t.Fatalf("final stderr line is not timing JSON: %q: %v", lines[len(lines)-1], jsonErr)
	}
	if report["runStatus"] != "failed" || report["errorKind"] != "provider-error" {
		t.Fatalf("timing report=%#v", report)
	}
}

func TestCloudRunSandboxRunEmitsTimingJSONOnWorkspaceSetupFailure(t *testing.T) {
	isolateLeaseHome(t)
	setupErr := errors.New("workspace unavailable")
	transport := &fakeTransport{
		mode: "direct",
		onExec: func(_ string, command string) (int, string, string, error) {
			if strings.Contains(command, "mkdir -p") {
				return 1, "", "", setupErr
			}
			return 0, "", "", nil
		},
	}
	previousTransport := newTransport
	newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })
	var stderr bytes.Buffer
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
		IdleTimeout:     time.Minute,
	}, core.Runtime{Stdout: io.Discard, Stderr: &stderr}).(*backend)
	_, err := b.Run(context.Background(), core.RunRequest{
		Repo:       core.Repo{Root: t.TempDir()},
		Command:    []string{"true"},
		NoSync:     true,
		TimingJSON: true,
	})
	if !errors.Is(err, setupErr) {
		t.Fatalf("run err=%v", err)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	var report map[string]any
	if jsonErr := json.Unmarshal([]byte(lines[len(lines)-1]), &report); jsonErr != nil {
		t.Fatalf("final stderr line is not timing JSON: %q: %v", lines[len(lines)-1], jsonErr)
	}
	if report["runStatus"] != "failed" || report["syncSkipped"] != true {
		t.Fatalf("timing report=%#v", report)
	}
}

func TestCloudRunSandboxStatusReportsExpiredClaim(t *testing.T) {
	isolateLeaseHome(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
	}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Clock: cloudRunSandboxFixedClock{now: now}}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	leaseID := leasePrefix + "expired-status"
	if err := claimTestCloudRunSandboxLease(leaseID, "expired", scope, t.TempDir(), time.Minute); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	labels := cloneLabels(claim.Labels)
	labels[claimExpiresAtLabel] = now.Add(-time.Second).Format(time.RFC3339Nano)
	if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, claim, labels); err != nil {
		t.Fatal(err)
	}
	status, err := b.Status(context.Background(), core.StatusRequest{ID: leaseID})
	if err != nil || status.Ready || status.State != "expired" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	leases, err := b.List(context.Background(), core.ListRequest{})
	if err != nil || len(leases) != 1 || leases[0].Status != "expired" {
		t.Fatalf("expired list=%#v err=%v", leases, err)
	}
}

func TestCloudRunSandboxClearActivityTouchesLastUsed(t *testing.T) {
	isolateLeaseHome(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
		IdleTimeout:     time.Minute,
	}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Clock: cloudRunSandboxFixedClock{now: now}}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	leaseID := leasePrefix + "touch-last-used"
	if err := claimTestCloudRunSandboxLease(leaseID, "touch", scope, t.TempDir(), time.Minute); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	claim, err = b.markClaimActivity(claim, "running", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err := b.clearClaimActivity(claim)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.LastUsedAt != now.Format(time.RFC3339) {
		t.Fatalf("lastUsedAt=%q want=%q", cleared.LastUsedAt, now.Format(time.RFC3339))
	}
	if due, reason := claimCleanupDue(cleared, now); due || reason != "idle-timeout-remaining" {
		t.Fatalf("freshly completed claim due=%v reason=%s", due, reason)
	}
}

func TestCloudRunSandboxReclaimCannotRepublishAfterCleanupWins(t *testing.T) {
	isolateLeaseHome(t)
	const sandboxID = "crabbox-cleanup-wins-123456"
	leaseID := leasePrefix + sandboxID
	destroyStarted := make(chan struct{})
	allowDestroy := make(chan struct{})
	transport := &fakeTransport{mode: "remote", onDestroy: func(string) error {
		close(destroyStarted)
		<-allowDestroy
		return nil
	}}
	previousTransport := newTransport
	newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })

	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"},
		IdleTimeout:     time.Second,
	}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	if err := claimTestCloudRunSandboxLease(leaseID, "cleanup-wins", scope, t.TempDir(), time.Second); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	expired := claim
	expired.LastUsedAt = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if err := core.ReplaceLeaseClaimIfUnchanged(leaseID, claim, expired); err != nil {
		t.Fatalf("expire claim: %v", err)
	}

	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- b.Cleanup(context.Background(), core.CleanupRequest{}) }()
	<-destroyStarted

	reclaimDone := make(chan error, 1)
	newRepo := t.TempDir()
	go func() {
		_, _, _, _, resolveErr := b.resolveLeaseID(leaseID, newRepo, true)
		reclaimDone <- resolveErr
	}()
	select {
	case reclaimErr := <-reclaimDone:
		t.Fatalf("reclaim bypassed cleanup claim lock: %v", reclaimErr)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowDestroy)
	if err := <-cleanupDone; err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if err := <-reclaimDone; err == nil || (!strings.Contains(err.Error(), "claim changed") && !strings.Contains(err.Error(), "not claimed by Crabbox")) {
		t.Fatalf("reclaim error=%v, want guarded missing-claim failure", err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("reclaim republished deleted sandbox claim: exists=%v err=%v", exists, err)
	}
}

func TestProviderSpec(t *testing.T) {
	t.Parallel()
	spec := Provider{}.Spec()
	if spec.Name != providerName {
		t.Fatalf("name=%q", spec.Name)
	}
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%v", spec.Kind)
	}
	for _, feature := range []core.Feature{core.FeatureArchiveSync, core.FeatureRunSession, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("missing feature %s in %v", feature, spec.Features)
		}
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%v", spec.Coordinator)
	}
}

func TestRunWithFakeTransport(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	fake := &fakeTransport{
		mode: "remote",
		onCreate: func(id string) error {
			mu.Lock()
			calls = append(calls, "create:"+id)
			mu.Unlock()
			return nil
		},
		onExec: func(_ string, command string) (int, string, string, error) {
			mu.Lock()
			calls = append(calls, "exec:"+command)
			mu.Unlock()
			if strings.Contains(command, "mkdir") {
				return 0, "", "", nil
			}
			return 0, "hello\n", "", nil
		},
		onDestroy: func(id string) error {
			mu.Lock()
			calls = append(calls, "destroy:"+id)
			mu.Unlock()
			return nil
		},
	}
	prev := newTransport
	newTransport = func(core.Config, core.Runtime) (sandboxTransport, error) { return fake, nil }
	t.Cleanup(func() { newTransport = prev })

	// Isolate lease claims to a temp dir.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	b := NewBackend(Provider{}.Spec(), core.Config{
		CloudRunSandbox: core.CloudRunSandboxConfig{
			CLIPath: "/usr/local/gcp/bin/sandbox",
			Workdir: "/tmp/crabbox",
			Write:   true,
		},
		IdleTimeout: 30 * time.Minute,
	}, core.Runtime{
		Stdout: &stdout,
		Stderr: &stderr,
	}).(*backend)

	result, err := b.Run(context.Background(), core.RunRequest{
		Repo:    core.Repo{Root: root},
		Command: []string{"echo", "hello"},
		NoSync:  true,
		Keep:    false,
	})
	if err != nil {
		t.Fatalf("Run: %v\nstderr=%s", err, stderr.String())
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d", result.ExitCode)
	}
	if !strings.Contains(stdout.String(), "hello") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(calls, ",")
	if !strings.Contains(joined, "create:") || !strings.Contains(joined, "exec:") || !strings.Contains(joined, "destroy:") {
		t.Fatalf("unexpected calls: %v", calls)
	}
}

func TestValidateConfig(t *testing.T) {
	t.Parallel()
	cfg := core.Config{CloudRunSandbox: core.CloudRunSandboxConfig{CLIPath: "/usr/local/gcp/bin/sandbox", Workdir: "/tmp/crabbox"}}
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cfg.CloudRunSandbox.Workdir = "relative"
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected relative workdir rejection")
	}
	cfg.CloudRunSandbox.Workdir = "/tmp/crabbox"
	cfg.CloudRunSandbox.CLIPath = ""
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected empty cliPath rejection")
	}
}

type fakeTransport struct {
	mode           string
	onCreate       func(string) error
	onProbe        func(string, string) error
	onExec         func(string, string) (int, string, string, error)
	onDestroy      func(string) error
	onDestroyOwned func(string, string) error
	onWrite        func(string, string) error
}

func claimTestCloudRunSandboxLease(leaseID, slug, scope, repoRoot string, idleTimeout time.Duration) error {
	_, err := core.ClaimLeaseForRepoProviderScopePondWithLabels(leaseID, slug, providerName, scope, "", repoRoot, idleTimeout, map[string]string{
		claimOwnershipLabel: strings.TrimPrefix(leaseID, leasePrefix),
	})
	return err
}

func (f *fakeTransport) Mode() string                 { return f.mode }
func (f *fakeTransport) Health(context.Context) error { return nil }

func (f *fakeTransport) Probe(_ context.Context, sandboxID, ownershipToken string) error {
	if f.onProbe != nil {
		return f.onProbe(sandboxID, ownershipToken)
	}
	return nil
}
func (f *fakeTransport) Create(_ context.Context, sandboxID string, _ runOptions) error {
	if f.onCreate != nil {
		return f.onCreate(sandboxID)
	}
	return nil
}
func (f *fakeTransport) Exec(_ context.Context, sandboxID, command string, _ execOptions, stdout, stderr io.Writer) (int, error) {
	if f.onExec != nil {
		code, out, errOut, err := f.onExec(sandboxID, command)
		if stdout != nil && out != "" {
			_, _ = io.WriteString(stdout, out)
		}
		if stderr != nil && errOut != "" {
			_, _ = io.WriteString(stderr, errOut)
		}
		return code, err
	}
	return 0, nil
}
func (f *fakeTransport) Destroy(_ context.Context, sandboxID, ownershipToken string) error {
	if f.onDestroyOwned != nil {
		return f.onDestroyOwned(sandboxID, ownershipToken)
	}
	if f.onDestroy != nil {
		return f.onDestroy(sandboxID)
	}
	return nil
}
func (f *fakeTransport) WriteFile(_ context.Context, sandboxID, path, _ string, _ bool) error {
	if f.onWrite != nil {
		return f.onWrite(sandboxID, path)
	}
	return nil
}
