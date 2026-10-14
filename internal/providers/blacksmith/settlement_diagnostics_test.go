package blacksmith

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestBlacksmithSettlementDiagnosticProcess(t *testing.T) {
	switch os.Getenv("CRABBOX_TEST_SETTLEMENT_DIAGNOSTIC") {
	case "exit":
		os.Exit(7)
	case "signal":
		process, err := os.FindProcess(os.Getpid())
		if err != nil {
			os.Exit(98)
		}
		_ = process.Kill()
		os.Exit(99)
	}
}

func TestBlacksmithGitHubFailureDiagnostics(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	observed := map[string]error{}
	for _, mode := range []string{"exit", "signal"} {
		if mode == "signal" && runtime.GOOS == "windows" {
			continue
		}
		result, err := core.RuntimeForProviderOperation(io.Discard).Exec.Run(t.Context(), core.LocalCommandRequest{
			Name: executable, Args: []string{"-test.run=^TestBlacksmithSettlementDiagnosticProcess$"},
			Env:                    []string{"CRABBOX_TEST_SETTLEMENT_DIAGNOSTIC=" + mode},
			MaxCapturedOutputBytes: 16384,
		})
		want := 7
		if mode == "signal" {
			want = -1
		}
		if err == nil || result.ExitCode != want {
			t.Fatalf("actual command fixture %s: exit=%d err=%v", mode, result.ExitCode, err)
		}
		observed[mode] = err
	}
	for _, method := range []string{"GET", "POST"} {
		for _, mode := range []string{"deadline", "canceled", "exit", "signal", "late-deadline", "unknown-process", "start", "other", "nonzero", "no-deadline"} {
			if (mode == "signal" || mode == "deadline" || mode == "canceled") && runtime.GOOS == "windows" {
				continue
			}
			t.Run(method+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					isolateBlacksmithOwnership(t)
					const id = "tbx_diagnostic"
					const runURL = "https://github.com/example-org/my-app/actions/runs/123456789"
					claim := seedStopClaim(t, id)
					ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
					if mode == "no-deadline" {
						cancel()
						ctx, cancel = context.WithCancel(t.Context())
					}
					defer cancel()
					deadline, hasDeadline := ctx.Deadline()
					calls, statuses := 0, 0
					var primary error
					var issued time.Time
					wantCause, wantProcess, wantSignal, wantContext := "other", "unknown", "unknown", "none"
					elapsed, budget := int64(25), "30000"
					code := 1
					cfg := core.BaseConfig()
					cfg.Blacksmith.Org = "example-org"
					backend := newTestBlacksmithBackend(cfg, settlementRunner(func(gotCtx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
						if gotCtx != ctx {
							t.Fatal("command received a different context")
						}
						if gotDeadline, ok := gotCtx.Deadline(); ok != hasDeadline || !gotDeadline.Equal(deadline) {
							t.Fatal("command budget changed")
						}
						if req.Name == "blacksmith" {
							statuses++
							return core.LocalCommandResult{Stdout: nativeStopStatusTable(id, "completed", "", claim.Labels["workflow"], claim.Labels["job"], claim.Labels["ref"], "2026-10-03T12:00:00Z", runURL)}, nil
						}
						calls++
						wantArgs := []string{"api", "--hostname", "github.com", "--method", method, "repos/example-org/my-app/actions/runs/123456789"}
						if method == "GET" {
							wantArgs = append(wantArgs, "--jq", "{id,html_url,status,conclusion}")
						} else {
							wantArgs[len(wantArgs)-1] += "/cancel"
						}
						if req.Name != "gh" || !reflect.DeepEqual(req.Args, wantArgs) || req.MaxCapturedOutputBytes != 16384 ||
							req.Env != nil || req.Stdout != nil || req.Stderr != nil || req.Stdin != nil {
							t.Fatalf("GitHub request contract changed: %+v", req)
						}
						issued = time.Now()
						time.Sleep(25 * time.Millisecond)
						primary = errors.New("synthetic-private-command-detail")
						switch mode {
						case "deadline", "late-deadline":
							time.Sleep(30*time.Second - 25*time.Millisecond)
							<-gotCtx.Done()
							elapsed, wantContext = 30000, "deadline"
							primary, code = observed["exit"], 7
							wantCause, wantProcess, wantSignal = "exit", "exited", "none"
							if mode == "deadline" {
								primary, code = errors.Join(observed["signal"], context.DeadlineExceeded), -1
								wantCause, wantProcess, wantSignal = "deadline", "signaled", "9"
							}
						case "canceled":
							cancel()
							primary, code = errors.Join(observed["signal"], context.Canceled), -1
							wantCause, wantProcess, wantSignal, wantContext = "canceled", "signaled", "9", "canceled"
						case "exit":
							primary, code = observed["exit"], 7
							wantCause, wantProcess, wantSignal = "exit", "exited", "none"
						case "signal":
							primary, code = observed["signal"], -1
							wantCause, wantProcess, wantSignal = "exit", "signaled", "9"
						case "unknown-process":
							primary, code = &exec.ExitError{}, -1
							wantCause = "exit"
						case "start":
							primary = &exec.Error{Name: "synthetic-private-command-detail", Err: os.ErrNotExist}
							wantCause = "start"
						case "other":
							primary = core.ExitError{Code: 17, Message: "synthetic-private-command-detail"}
						case "nonzero":
							primary, code, wantCause = nil, 9, "none"
						case "no-deadline":
							budget = "unknown"
						}
						return core.LocalCommandResult{ExitCode: code, Stdout: "synthetic-private-output", Stderr: "synthetic-private-stderr"}, primary
					}))
					backend.route = &blacksmithRoute{API: blacksmithDefaultAPI, Org: "example-org"}
					var err error
					if method == "GET" {
						_, err = backend.githubRunConclusion(ctx, runURL)
					} else {
						var attempted bool
						attempted, err = backend.cancelGitHubRun(ctx, claim, runURL)
						if !attempted {
							t.Fatal("dispatched cancellation lost its receipt")
						}
					}
					if err == nil || primary != nil && !errors.Is(err, primary) {
						t.Fatalf("primary cause lost: %v", err)
					}
					if ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
						t.Fatal("context cause lost")
					}
					var displayed core.ExitError
					if !core.AsExitError(err, &displayed) {
						t.Fatal("safe display boundary missing")
					}
					for _, text := range []string{err.Error(), displayed.Message} {
						if strings.Contains(text, "synthetic-private") {
							t.Fatal("private command diagnostics leaked")
						}
						for _, fact := range []string{
							fmt.Sprintf("gh exit=%d", code), "cause=" + wantCause, "process=" + wantProcess,
							"signal=" + wantSignal, "issued_at=" + issued.UTC().Format(time.RFC3339Nano),
							fmt.Sprintf("elapsed_ms=%d", elapsed), "remaining_budget_ms=" + budget, "context_after=" + wantContext,
						} {
							if !strings.Contains(text, fact) {
								t.Errorf("missing %s in %s", fact, text)
							}
						}
					}
					wantStatuses, wantCode := 0, 5
					if method == "POST" {
						wantStatuses, wantCode = 1, 1
					}
					if calls != 1 || statuses != wantStatuses || displayed.Code != wantCode {
						t.Fatalf("calls=%d statuses=%d display exit=%d", calls, statuses, displayed.Code)
					}
					assertStopState(t, claim, true)
				})
			})
		}
	}
}

func BenchmarkBlacksmithGitHubSettlementFailure(b *testing.B) {
	failure := errors.New("private command diagnostic")
	backend := newTestBlacksmithBackend(core.BaseConfig(), settlementRunner(func(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error) {
		return core.LocalCommandResult{ExitCode: -1}, failure
	}))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := backend.githubRunConclusion(context.Background(), "https://github.com/example-org/my-app/actions/runs/123456789"); err == nil {
			b.Fatal("missing error")
		}
	}
}
