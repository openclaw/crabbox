package blacksmith

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func diagnosticProcessFailure(t *testing.T, mode string) error {
	t.Helper()
	if mode == "signal" && runtime.GOOS == "windows" {
		t.Skip("signal process status is a Unix contract")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	result, err := core.RuntimeForProviderOperation(io.Discard).Exec.Run(t.Context(), core.LocalCommandRequest{
		Name: executable, Args: []string{"-test.run=^TestBlacksmithSettlementDiagnosticProcess$"},
		Env: []string{"CRABBOX_TEST_SETTLEMENT_DIAGNOSTIC=" + mode}, MaxCapturedOutputBytes: 16384,
	})
	if err == nil || mode == "signal" && result.ExitCode != -1 || mode == "exit" && result.ExitCode != 7 {
		t.Fatalf("actual process fixture: result=%+v err=%v", result, err)
	}
	return err
}

func TestBlacksmithCleanupSeparatesCommandAndFinalizationFailures(t *testing.T) {
	signalErr := diagnosticProcessFailure(t, "signal")
	for _, mode := range []string{"admission", "final-verification", "reconciliation"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				isolateBlacksmithOwnership(t)
				claim := seedStopClaim(t, "tbx_cleanup_phase")
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()
				deadline, _ := ctx.Deadline()
				release, held, joined := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				if mode == "admission" {
					go func() {
						joined <- core.WithLeaseClaimUnchangedShared(t.Context(), claim.LeaseID, claim, func() error {
							close(held)
							<-release
							return nil
						})
					}()
					<-held
					defer func() {
						close(release)
						if err := <-joined; err != nil {
							t.Error(err)
						}
					}()
				}
				var calls []string
				statuses := 0
				cfg := core.BaseConfig()
				cfg.Blacksmith.Org = "example-org"
				backend := newTestBlacksmithBackend(cfg, settlementRunner(func(gotCtx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
					gotDeadline, ok := gotCtx.Deadline()
					if !ok || !gotDeadline.Equal(deadline) {
						t.Fatal("cleanup changed the inherited deadline")
					}
					if req.Name == "gh" {
						calls = append(calls, "github")
						result, ok := testCompletedGitHubRead(req)
						if !ok {
							t.Fatal("unexpected GitHub request")
						}
						return result, nil
					}
					if req.Name != "blacksmith" {
						t.Fatal("unexpected command")
					}
					args := req.Args
					if len(args) > 2 && args[0] == "--org" {
						args = args[2:]
					}
					calls = append(calls, args[1])
					switch args[1] {
					case "stop":
						if gotCtx.Err() != nil {
							t.Fatal("stop fixture must fail while its context is live")
						}
						if mode != "final-verification" {
							return core.LocalCommandResult{ExitCode: -1}, signalErr
						}
						return core.LocalCommandResult{}, nil
					case "status":
						statuses++
						if statuses == 1 {
							return core.LocalCommandResult{Stdout: nativeStopStatus(claim.LeaseID, "ready", "")}, nil
						}
						if mode == "final-verification" && statuses == 3 || mode == "reconciliation" && statuses == 2 {
							<-gotCtx.Done()
							return core.LocalCommandResult{ExitCode: -1}, errors.Join(signalErr, context.DeadlineExceeded)
						}
						return core.LocalCommandResult{Stdout: nativeStopStatus(claim.LeaseID, "completed", "")}, nil
					default:
						t.Fatal("unexpected native operation")
						return core.LocalCommandResult{}, nil
					}
				}))
				err := backend.Stop(ctx, core.StopRequest{ID: claim.LeaseID})
				var exit core.ExitError
				if !core.AsExitError(err, &exit) || exit.Code != -1 || !errors.Is(err, signalErr) || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("cleanup lost selected exit or causes: %v", err)
				}
				want := []string{"status", "stop", "status", "github"}
				phase := "finalization-admission"
				if mode == "final-verification" {
					want = append(want, "status")
					phase = "final-verification"
				} else if mode == "reconciliation" {
					want = want[:3]
					phase = "reconciliation"
				}
				if !reflect.DeepEqual(calls, want) || time.Now() != deadline {
					t.Fatalf("cleanup order or deadline changed: calls=%v time=%s", calls, time.Now())
				}
				if !strings.Contains(err.Error(), "phase="+phase) {
					t.Errorf("missing producer phase %s: %v", phase, err)
				}
				if mode != "final-verification" && !strings.Contains(err.Error(), "blacksmith testbox stop exit=-1 cause=exit process=signaled signal=9") {
					t.Errorf("earlier stop cause was relabeled: %v", err)
				}
				if mode != "admission" && !strings.Contains(err.Error(), "blacksmith testbox status exit=-1 cause=deadline process=signaled signal=9") {
					t.Errorf("status cause unavailable: %v", err)
				}
				assertStopState(t, claim, true)
			})
		})
	}
}

func TestBlacksmithNativeCommandDiagnosticBoundary(t *testing.T) {
	ordinaryExit := diagnosticProcessFailure(t, "exit")
	for _, operation := range []string{"status", "stop"} {
		for _, mode := range []string{"late-deadline", "private-cause", "nonzero", "success"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
					defer cancel()
					issued := time.Now()
					calls, code := 0, 7
					cause := ordinaryExit
					if mode == "private-cause" {
						cause = core.Exit(17, "synthetic-private-command-detail")
					} else if mode == "nonzero" || mode == "success" {
						cause = nil
						if mode == "success" {
							code = 0
						}
					}
					args := []string{"testbox", operation, "--id", "tbx_native_diagnostic"}
					backend := newTestBlacksmithBackend(core.BaseConfig(), settlementRunner(func(gotCtx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
						calls++
						if gotCtx != ctx || req.Name != "blacksmith" || !reflect.DeepEqual(req.Args, args) ||
							req.MaxCapturedOutputBytes != blacksmithCommandCaptureBytes || req.Env != nil || req.Stdout != nil || req.Stderr != nil {
							t.Fatalf("native request changed: %+v", req)
						}
						if mode == "late-deadline" {
							<-ctx.Done()
						}
						return core.LocalCommandResult{ExitCode: code, Stderr: "synthetic-private-stderr"}, cause
					}))
					result, err := backend.runCommand(ctx, args, nil, nil)
					if result.ExitCode != code || calls != 1 || cause != nil && !errors.Is(err, cause) {
						t.Fatalf("native result/cause changed: %+v %v", result, err)
					}
					if mode == "success" {
						if err != nil {
							t.Fatal(err)
						}
						return
					}
					if err == nil {
						t.Fatal("missing native failure")
					}
					if strings.Contains(err.Error(), "synthetic-private") {
						t.Errorf("private diagnostic leaked: %v", err)
					}
					for _, fact := range []string{"blacksmith testbox " + operation, "exit=7", "issued_at=" + issued.UTC().Format(time.RFC3339Nano), "remaining_budget_ms=30000"} {
						if !strings.Contains(err.Error(), fact) {
							t.Errorf("missing %s: %v", fact, err)
						}
					}
					if mode == "late-deadline" && (!strings.Contains(err.Error(), "cause=exit process=exited signal=none") || !strings.Contains(err.Error(), "context_after=deadline") || !strings.Contains(err.Error(), "elapsed_ms=30000")) {
						t.Errorf("post-return expiry relabeled ordinary exit: %v", err)
					}
				})
			})
		}
	}
}

func BenchmarkBlacksmithNativeStatusFailure(b *testing.B) {
	failure := errors.New("private native command detail")
	backend := newTestBlacksmithBackend(core.BaseConfig(), settlementRunner(func(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error) {
		return core.LocalCommandResult{ExitCode: -1}, failure
	}))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := backend.inspectTestbox(context.Background(), "tbx_diagnostic"); err == nil {
			b.Fatal(fmt.Errorf("missing native failure"))
		}
	}
}

func BenchmarkBlacksmithGitHubCommandSuccess(b *testing.B) {
	backend := newTestBlacksmithBackend(core.BaseConfig(), settlementRunner(func(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error) {
		return core.LocalCommandResult{}, nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := []string{"api", "--hostname", "github.com", "--method", "GET", "repos/example-org/my-app/actions/runs/123456789"}
	b.ReportAllocs()
	for b.Loop() {
		if _, err, diagnostic := backend.githubCommand(ctx, args); err != nil || diagnostic != "" {
			b.Fatalf("successful command: %v %q", err, diagnostic)
		}
	}
}
