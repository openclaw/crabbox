package blacksmith

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"text/tabwriter"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

const stopDiagnostic = "Error: stop failed: HTTP 409: testbox already stopped\n"

func nativeStopStatus(id, state, ip string) string {
	return nativeStopStatusTable(id, state, ip, ".github/workflows/testbox.yml", "test", "main",
		"2026-08-30T12:00:00.123456Z", "https://github.com/example-org/my-app/actions/runs/123456789")
}

func nativeStopStatusTable(cells ...string) string {
	var table strings.Builder
	w := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tIP\tWORKFLOW\tJOB\tREF\tCREATED\tRUN URL")
	fmt.Fprintln(w, strings.Join(cells, "\t"))
	w.Flush()
	return table.String()
}

func seedStopClaim(t *testing.T, id string) core.LeaseClaim {
	t.Helper()
	testOwnedBlacksmithClaim(t, id, "stop-"+strings.TrimPrefix(id, "tbx_"), t.TempDir())
	key, err := testboxKeyPath(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("synthetic-key-"+id), 0o600); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(id)
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func assertStopState(t *testing.T, claim core.LeaseClaim, retained bool) {
	t.Helper()
	got, err := readLeaseClaim(claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if retained {
		if !reflect.DeepEqual(got, claim) {
			t.Fatalf("claim changed: got %#v, want %#v", got, claim)
		}
	} else if got.LeaseID != "" {
		t.Fatalf("claim retained: %#v", got)
	}
	key, err := testboxKeyPath(claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(key)
	if retained {
		if err != nil || string(data) != "synthetic-key-"+claim.LeaseID {
			t.Fatalf("stored key changed: data=%q err=%v", data, err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("key retained: %v", err)
	}
}

func TestBlacksmithStopReconcilesCompletedClaim(t *testing.T) {
	const id = "tbx_completed123"
	for _, mode := range []string{"with-ip", "empty-ip", "unicode-workflow", "successful-stop"} {
		t.Run(mode, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			claim := seedStopClaim(t, id)
			unrelated := seedStopClaim(t, "tbx_unrelated123")
			if mode == "unicode-workflow" {
				changed := claim
				changed.Labels = maps.Clone(claim.Labels)
				changed.Labels["workflow"] = "Build  and test café"
				if err := core.ReplaceLeaseClaimIfUnchanged(id, claim, changed); err != nil {
					t.Fatal(err)
				}
				claim, _ = readLeaseClaim(id)
			}
			var stdout, stderr bytes.Buffer
			stopped := false
			var operations []string
			cfg := baseConfig()
			cfg.Blacksmith.Org = "example-org"
			backend := newTestBlacksmithBackend(cfg, reconciliationRunner(t, func(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
				operations = append(operations, req.Args[1])
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > blacksmithCleanupTimeout {
					t.Error("unbounded stop context")
				}
				switch req.Args[1] {
				case "status":
					state, ip := "ready", "192.0.2.10"
					if stopped {
						state = "completed"
					}
					if mode != "with-ip" {
						ip = ""
					}
					return LocalCommandResult{Stdout: nativeStopStatusTable(id, state, ip, claim.Labels["workflow"], "test", "main", "2026-08-30T12:00:00.123456Z", "https://github.com/example-org/my-app/actions/runs/123456789")}, nil
				case "stop":
					stopped = true
					if mode == "successful-stop" {
						return LocalCommandResult{Stdout: "stopped\n", Stderr: "stop note\n"}, nil
					}
					return LocalCommandResult{ExitCode: 1, Stderr: stopDiagnostic}, errors.New("exit status 1")
				default:
					return LocalCommandResult{}, errors.New("unexpected operation")
				}
			}))
			backend.rt.Stdout, backend.rt.Stderr = &stdout, &stderr
			if err := backend.Stop(t.Context(), StopRequest{ID: claim.Slug}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(operations, []string{"status", "stop", "status", "status"}) {
				t.Fatalf("operations=%v", operations)
			}
			assertStopState(t, claim, false)
			assertStopState(t, unrelated, true)
			if mode == "successful-stop" {
				if stdout.String() != "stopped\n" || stderr.String() != "stop note\n" {
					t.Fatalf("output changed: %q %q", stdout.String(), stderr.String())
				}
			} else if !strings.Contains(stderr.String(), "cleanup reconciled lease="+id+" state=completed") || strings.Contains(stderr.String(), stopDiagnostic) {
				t.Fatalf("diagnostics=%q", stderr.String())
			}
		})
	}
}

func TestBlacksmithStopReconciliationFailsClosed(t *testing.T) {
	const id = "tbx_completed123"
	completed := nativeStopStatus(id, "completed", "192.0.2.10")
	header, row, _ := strings.Cut(completed, "\n")
	header += "\n"
	blankIP := nativeStopStatus(id, "completed", "")
	blankHeader, blankRow, _ := strings.Cut(blankIP, "\n")
	type statusCase struct {
		name     string
		stdout   string
		stderr   string
		code     int
		err      error
		cancelAt string
		deadline bool
	}
	tests := []statusCase{
		{name: "no output"},
		{name: "header only", stdout: header},
		{name: "no header", stdout: row},
		{name: "missing status", stdout: strings.Replace(completed, "completed", strings.Repeat(" ", len("completed")), 1)},
		{name: "wrong header", stdout: strings.Replace(completed, "IP ", "REPO ", 1)},
		{name: "legacy list", stdout: id + " completed my-app testbox.yml test main 2026-08-30T12:00:00Z\n"},
		{name: "short row", stdout: header + id + "  completed\n"},
		{name: "truncated row", stdout: strings.TrimSuffix(completed, "https://github.com/example-org/my-app/actions/runs/123456789\n")},
		{name: "truncated run URL", stdout: strings.Replace(completed, "/runs/123456789", "/runs/", 1)},
		{name: "extra cell", stdout: strings.TrimSpace(completed) + "  unexpected\n"},
		{name: "duplicate row", stdout: completed + row},
		{name: "conflicting row", stdout: completed + strings.Replace(row, "completed", "ready    ", 1)},
		{name: "different row", stdout: completed + strings.Replace(row, id, "tbx_other", 1)},
		{name: "wrong id", stdout: nativeStopStatus("tbx_other", "completed", "")},
		{name: "id prefix", stdout: nativeStopStatus(id+"extra", "completed", "")},
		{name: "id suffix", stdout: nativeStopStatus("prefix"+id, "completed", "")},
		{name: "substring id", stdout: nativeStopStatus("["+id+"]", "completed", "")},
		{name: "blank IP collapsed whitespace", stdout: blankHeader + "\n" + strings.Join(strings.Fields(blankRow), "  ") + "\n"},
		{name: "blank IP truncated row", stdout: strings.TrimSuffix(blankIP, "https://github.com/example-org/my-app/actions/runs/123456789\n")},
		{name: "blank IP truncated run URL", stdout: strings.Replace(blankIP, "/runs/123456789", "/runs/", 1)},
		{name: "blank IP extra cell", stdout: strings.TrimSuffix(blankIP, "\n") + "  unexpected\n"},
		{name: "blank IP misaligned row", stdout: strings.Replace(completed, "192.0.2.10", "", 1)},
		{name: "stderr table only", stderr: completed},
		{name: "blank IP stderr table only", stderr: blankIP},
		{name: "stderr terminal prose", stderr: id + " completed"},
		{name: "stderr cannot complete stdout", stdout: header, stderr: row},
		{name: "error prose", stdout: "Error: " + id + " is completed (HTTP 409)\n"},
		{name: "error after table", stdout: completed + "Error: status lookup failed\n"},
		{name: "error as row", stdout: header + id + " completed but status lookup failed; please retry\n"},
		{name: "404 absence", stdout: "Testbox not found (HTTP 404)\n"},
		{name: "json unsupported", stdout: `{"id":"tbx_completed123","status":"completed"}`},
		{name: "status exit 7", stdout: completed, code: 7, err: errors.New("exit status 7")},
		{name: "status exit 255", stdout: completed, code: 255, err: errors.New("exit status 255")},
		{name: "status unavailable", code: 1, err: errors.New("executable file not found")},
		{name: "capture overflow", stdout: completed, code: 5, err: errors.New("captured command output exceeded limit")},
		{name: "query canceled", stdout: completed, code: 1, err: context.Canceled, cancelAt: "status"},
		{name: "query deadline", stdout: completed, code: 1, err: context.DeadlineExceeded, deadline: true},
		{name: "canceled after successful status", stdout: completed, cancelAt: "status"},
		{name: "canceled after stop", stdout: completed, cancelAt: "stop"},
	}
	cells := []string{id, "completed", "", ".github/workflows/testbox.yml", "test", "main", "2026-08-30T12:00:00.123456Z", "https://github.com/example-org/my-app/actions/runs/123456789"}
	for i, name := range []string{"ID", "STATUS", "IP", "WORKFLOW", "JOB", "REF", "CREATED", "RUN URL"} {
		if name == "IP" {
			continue
		}
		missing := append([]string(nil), cells...)
		missing[i] = ""
		tests = append(tests, statusCase{name: "blank IP missing " + name, stdout: nativeStopStatusTable(missing...)})
		removed := append(append([]string(nil), cells[:i]...), cells[i+1:]...)
		tests = append(tests, statusCase{name: "blank IP removed " + name + " column", stdout: nativeStopStatusTable(removed...)})
	}
	for i := range cells {
		extra := append(append(append([]string(nil), cells[:i]...), "unexpected"), cells[i:]...)
		tests = append(tests, statusCase{name: fmt.Sprintf("blank IP extra column %d", i), stdout: nativeStopStatusTable(extra...)})
	}
	for _, state := range []string{"ready", "queued", "hydrating", "running", "in_progress", "unknown", "failed", "cancelled", "canceled", "stopped", "terminated", "Completed", "COMPLETED", "!Ready"} {
		tests = append(tests, statusCase{name: state, stdout: nativeStopStatus(id, state, "")})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			claim := seedStopClaim(t, id)
			var stdout, stderr bytes.Buffer
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.deadline {
				var done context.CancelFunc
				ctx, done = context.WithTimeout(ctx, 100*time.Millisecond)
				defer done()
			}
			stopped := false
			var operations []string
			cfg := baseConfig()
			cfg.Blacksmith.Org = "example-org"
			backend := newTestBlacksmithBackend(cfg, reconciliationRunner(t, func(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
				operations = append(operations, req.Args[1])
				if req.Args[1] == "status" && !stopped {
					return LocalCommandResult{Stdout: nativeStopStatus(id, "ready", "")}, nil
				}
				if req.Args[1] == tt.cancelAt {
					cancel()
				}
				switch req.Args[1] {
				case "stop":
					stopped = true
					return LocalCommandResult{ExitCode: 1, Stdout: "stop stdout\n", Stderr: stopDiagnostic}, errors.New("exit status 1")
				case "status":
					if tt.deadline {
						<-ctx.Done()
					}
					return LocalCommandResult{ExitCode: tt.code, Stdout: tt.stdout, Stderr: tt.stderr}, tt.err
				default:
					return LocalCommandResult{}, errors.New("unexpected operation")
				}
			}))
			backend.rt.Stdout, backend.rt.Stderr = &stdout, &stderr
			err := backend.Stop(ctx, StopRequest{ID: id})
			var exitErr ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != 1 || exitErr.Message != "blacksmith failed: exit status 1" {
				t.Fatalf("original stop error lost: %v", err)
			}
			want := []string{"status", "stop", "status"}
			if tt.cancelAt == "stop" {
				want = want[:2]
			}
			if !reflect.DeepEqual(operations, want) {
				t.Fatalf("operations=%v want=%v", operations, want)
			}
			assertStopState(t, claim, true)
			if stdout.String() != "stop stdout\n" || stderr.String() != stopDiagnostic {
				t.Fatalf("original diagnostics lost: %q %q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestBlacksmithStopRejectsForeignProviderClaim(t *testing.T) {
	isolateBlacksmithOwnership(t)
	claim := seedStopClaim(t, "tbx_foreign123")
	foreign := claim
	foreign.Provider = "ssh"
	if err := core.ReplaceLeaseClaimIfUnchanged(claim.LeaseID, claim, foreign); err != nil {
		t.Fatal(err)
	}
	foreign, _ = readLeaseClaim(claim.LeaseID)
	backend := newTestBlacksmithBackend(baseConfig(), ownershipRunner(func(context.Context, LocalCommandRequest) (LocalCommandResult, error) {
		t.Error("foreign claim reached provider")
		return LocalCommandResult{}, nil
	}))
	if err := backend.Stop(t.Context(), StopRequest{ID: claim.LeaseID}); err == nil {
		t.Fatal("foreign claim stopped")
	}
	assertStopState(t, foreign, true)
}

func TestBlacksmithReconciliationRetainsOriginalErrorUntilFinalized(t *testing.T) {
	for _, mode := range []string{"status-error", "cancelled", "changed-identity", "not-completed"} {
		t.Run(mode, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			claim := seedStopClaim(t, "tbx_finalization123")
			var stdout, stderr bytes.Buffer
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			inspections := 0
			cfg := baseConfig()
			cfg.Blacksmith.Org = "example-org"
			backend := newTestBlacksmithBackend(cfg, reconciliationRunner(t, func(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
				if req.Args[1] == "stop" {
					return LocalCommandResult{ExitCode: 1, Stdout: "stop stdout\n", Stderr: stopDiagnostic}, errors.New("exit status 1")
				}
				inspections++
				state := "completed"
				if inspections == 1 {
					state = "ready"
				}
				output := nativeStopStatus(claim.LeaseID, state, "")
				if inspections == 3 {
					switch mode {
					case "status-error":
						return LocalCommandResult{ExitCode: 7}, errors.New("final verification unavailable")
					case "cancelled":
						cancel()
					case "changed-identity":
						output = strings.ReplaceAll(output, "testbox.yml", "foreign.yml")
					case "not-completed":
						output = nativeStopStatus(claim.LeaseID, "ready", "")
					}
				}
				return LocalCommandResult{Stdout: output}, nil
			}))
			backend.rt.Stdout, backend.rt.Stderr = &stdout, &stderr
			err := backend.Stop(ctx, StopRequest{ID: claim.LeaseID})
			var nativeErr ExitError
			if !errors.As(err, &nativeErr) || nativeErr.Code != 1 || nativeErr.Message != "blacksmith failed: exit status 1" || inspections != 3 {
				t.Fatalf("original failure lost after finalization: err=%v inspections=%d", err, inspections)
			}
			assertStopState(t, claim, true)
			if stdout.String() != "stop stdout\n" || stderr.String() != stopDiagnostic {
				t.Fatalf("original diagnostics lost: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestBlacksmithStopRejectsChangedClaimBeforeProviderCalls(t *testing.T) {
	for _, sameValue := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_value=%t", sameValue), func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			claim := seedStopClaim(t, "tbx_replaced123")
			replacement := claim
			if !sameValue {
				replacement.RepoRoot = t.TempDir()
			}
			if err := core.ReplaceLeaseClaimIfUnchanged(claim.LeaseID, claim, replacement); err != nil {
				t.Fatal(err)
			}
			rewritten, err := readLeaseClaim(claim.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if rewritten.Revision == claim.Revision {
				t.Fatal("rewrite failed to change revision")
			}
			replacement.Revision = rewritten.Revision
			if !reflect.DeepEqual(replacement, rewritten) {
				t.Fatalf("unexpected rewrite: got %#v want %#v", rewritten, replacement)
			}
			runner := &blacksmithFuncRunner{}
			backend := newTestBlacksmithBackend(baseConfig(), runner)
			err = backend.stopClaimedTestbox(t.Context(), claim.LeaseID, claim)
			if err == nil || !strings.Contains(err.Error(), "claim changed; retry") || len(runner.calls) != 0 {
				t.Fatalf("replacement was not fenced: err=%v calls=%v", err, runner.calls)
			}
			assertStopState(t, rewritten, true)
		})
	}
}

func TestBlacksmithStopHoldsClaimFenceDuringStatus(t *testing.T) {
	isolateBlacksmithOwnership(t)
	claim := seedStopClaim(t, "tbx_fenced123")
	entered, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(proceed) }) }
	defer release()
	stopped := false
	inspections := 0
	cfg := baseConfig()
	cfg.Blacksmith.Org = "example-org"
	backend := newTestBlacksmithBackend(cfg, reconciliationRunner(t, func(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
		switch req.Args[1] {
		case "stop":
			stopped = true
			return LocalCommandResult{ExitCode: 1, Stderr: stopDiagnostic}, errors.New("exit status 1")
		case "status":
			inspections++
			if !stopped {
				return LocalCommandResult{Stdout: nativeStopStatus(claim.LeaseID, "ready", "")}, nil
			}
			if inspections == 2 {
				close(entered)
				select {
				case <-proceed:
				case <-ctx.Done():
					return LocalCommandResult{}, ctx.Err()
				}
			}
			return LocalCommandResult{Stdout: nativeStopStatus(claim.LeaseID, "completed", "")}, nil
		default:
			return LocalCommandResult{}, errors.New("unexpected command")
		}
	}))
	done := make(chan error, 1)
	go func() { done <- backend.stopClaimedTestbox(t.Context(), claim.LeaseID, claim) }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("stop ended before reconciliation: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("status never entered")
	}
	writer := make(chan error, 1)
	go func() {
		_, err := core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, map[string]string{"state": "ready"})
		writer <- err
	}()
	select {
	case err := <-writer:
		t.Fatalf("writer crossed remote fence: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	stopErr, writeErr := <-done, <-writer
	if writeErr == nil {
		if stopErr == nil {
			t.Fatal("replacement was deleted")
		}
		after, _ := readLeaseClaim(claim.LeaseID)
		assertStopState(t, after, true)
	} else {
		if stopErr != nil {
			t.Fatal(stopErr)
		}
		assertStopState(t, claim, false)
	}
}

func TestBlacksmithClaimlessStopRefusesProviderAccess(t *testing.T) {
	isolateBlacksmithOwnership(t)
	backend := newTestBlacksmithBackend(baseConfig(), ownershipRunner(func(context.Context, LocalCommandRequest) (LocalCommandResult, error) {
		t.Error("unclaimed ID reached provider")
		return LocalCommandResult{}, nil
	}))
	if err := backend.Stop(t.Context(), StopRequest{ID: "tbx_raw123"}); err == nil {
		t.Fatal("unclaimed stop succeeded")
	}
}

func TestBlacksmithOneShotReconciliationPreservesCommandResult(t *testing.T) {
	for _, command := range []struct {
		name   string
		code   int
		output string
	}{
		{"success", 0, "tests passed\n"},
		{"transport", 255, "ssh: connection reset by peer\n"},
		{"test failure 255", 255, "FAIL: assertion mismatch\n"},
		{"test failure 1", 1, "FAIL: assertion mismatch\n"},
		{"test failure 7", 7, "FAIL: assertion mismatch\n"},
		{"not found", 127, "sh: test-runner: command not found\n"},
	} {
		for _, state := range []string{"completed", "ready"} {
			t.Run(command.name+"/"+state, func(t *testing.T) {
				testutil.IsolateUserDirs(t)
				repo := t.TempDir()
				t.Chdir(repo)
				const id = "tbx_oneshot123"
				var stderr bytes.Buffer
				stopped := false
				runner := reconciliationRunner(t, func(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
					switch req.Args[1] {
					case "warmup":
						return LocalCommandResult{Stdout: id + "\n"}, nil
					case "run":
						fmt.Fprint(req.Stderr, command.output)
						if command.code != 0 {
							return LocalCommandResult{ExitCode: command.code}, fmt.Errorf("exit status %d", command.code)
						}
						return LocalCommandResult{}, nil
					case "stop":
						stopped = true
						return LocalCommandResult{ExitCode: 1, Stderr: stopDiagnostic}, errors.New("exit status 1")
					case "status":
						if !stopped {
							return LocalCommandResult{Stdout: nativeStopStatus(id, "ready", "")}, nil
						}
						deadline, ok := ctx.Deadline()
						if !ok || time.Until(deadline) > blacksmithCleanupTimeout {
							t.Error("unbounded cleanup context")
						}
						return LocalCommandResult{Stdout: nativeStopStatus(id, state, "")}, nil
					default:
						t.Fatalf("unexpected command: %v", req.Args)
						return LocalCommandResult{}, nil
					}
				})
				cfg := baseConfig()
				cfg.Blacksmith.Org = "example-org"
				cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
				backend := newTestBlacksmithBackend(cfg, runner)
				backend.rt.Stderr = &stderr
				result, err := backend.Run(t.Context(), RunRequest{Repo: Repo{Root: repo}, Command: []string{"test-runner"}, TimingJSON: true})
				wantCode := command.code
				if state != "completed" && wantCode == 0 {
					wantCode = 1
				}
				var exitErr ExitError
				if wantCode == 0 {
					if err != nil {
						t.Fatal(err)
					}
				} else if !errors.As(err, &exitErr) || exitErr.Code != wantCode || exitErr.Message != fmt.Sprintf("blacksmith testbox run exited %d", wantCode) {
					t.Fatalf("command error changed: %v", err)
				}
				if result.ExitCode != wantCode || result.LeaseID != id {
					t.Fatalf("result=%#v", result)
				}
				var reports []core.TimingReport
				for _, line := range strings.Split(stderr.String(), "\n") {
					if strings.HasPrefix(line, "{") {
						var report core.TimingReport
						if err := json.Unmarshal([]byte(line), &report); err != nil {
							t.Fatal(err)
						}
						reports = append(reports, report)
					}
				}
				if len(reports) != 1 || reports[0].ExitCode != wantCode || reports[0].LeaseID != id || reports[0].CommandMs != result.Command.Milliseconds() || reports[0].TotalMs != result.Total.Milliseconds() {
					t.Fatalf("timing changed: %#v; result=%#v", reports, result)
				}
				claim, err := readLeaseClaim(id)
				if err != nil {
					t.Fatal(err)
				}
				key, err := testboxKeyPath(id)
				if err != nil {
					t.Fatal(err)
				}
				_, keyErr := os.Stat(key)
				if state == "completed" {
					if claim.LeaseID != "" || !os.IsNotExist(keyErr) || strings.Contains(stderr.String(), "cleanup failed") || strings.Contains(stderr.String(), stopDiagnostic) {
						t.Fatalf("reconciliation failed: claim=%#v key=%v stderr=%s", claim, keyErr, stderr.String())
					}
				} else if claim.LeaseID != id || keyErr != nil || !strings.Contains(stderr.String(), "cleanup failed") || !strings.Contains(stderr.String(), stopDiagnostic) {
					t.Fatalf("unconfirmed cleanup changed: claim=%#v key=%v stderr=%s", claim, keyErr, stderr.String())
				}
				if command.code != 0 && !strings.Contains(stderr.String(), command.output) {
					t.Fatal("command failure output was hidden")
				}
			})
		}
	}
}

// Keep and reuse must never reach either stop or its reconciliation query.
func TestBlacksmithReconciliationRespectsKeepAndReuse(t *testing.T) {
	for _, mode := range []string{"keep", "keep-on-failure", "reuse"} {
		t.Run(mode, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			repo := t.TempDir()
			t.Chdir(repo)
			const id = "tbx_kept123"
			runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
				switch req.Args[1] {
				case "warmup":
					return LocalCommandResult{Stdout: id + "\n"}, nil
				case "run":
					return LocalCommandResult{ExitCode: 255}, errors.New("exit status 255")
				default:
					t.Fatalf("kept/reused testbox reached cleanup: %v", req.Args)
					return LocalCommandResult{}, nil
				}
			}}
			cfg := baseConfig()
			cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
			req := RunRequest{Repo: Repo{Root: repo}, Command: []string{"false"}, Keep: mode == "keep", KeepOnFailure: mode == "keep-on-failure"}
			if mode == "reuse" {
				req.ID = id
				testOwnedBlacksmithClaim(t, id, "kept", repo)
			}
			backend := newTestBlacksmithBackend(cfg, runner)
			backend.rt.Stderr = io.Discard
			result, err := backend.Run(t.Context(), req)
			var exitErr ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != 255 || result.ExitCode != 255 || result.Session == nil || !result.Session.Kept {
				t.Fatalf("kept result=%#v err=%v", result, err)
			}
			claim, err := readLeaseClaim(id)
			if err != nil || claim.LeaseID != id {
				t.Fatalf("kept claim=%#v err=%v", claim, err)
			}
		})
	}
}

func reconciliationRunner(t *testing.T, fn func(context.Context, LocalCommandRequest) (LocalCommandResult, error)) ownershipRunner {
	t.Helper()
	return func(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
		if testBlacksmithFlag(req.Args, "--org") != "example-org" || testBlacksmithFlag(req.Args, "--api-url") != "https://backend.blacksmith.sh" {
			t.Error("native operation lost exact route")
		}
		if len(req.Args) > 2 && req.Args[0] == "--org" {
			req.Args = req.Args[2:]
		}
		return fn(ctx, req)
	}
}
