package blacksmith

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func waitCleanupBudget(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func TestBlacksmithCleanupPhaseBudget(t *testing.T) {
	for _, mode := range []string{"success", "success-native-error", "replacement", "late-denied", "caller-deadline", "caller-canceled", "final-denied", "final-pending", "final-changed-url", "final-nonterminal", "final-deadline", "final-canceled", "final-admission"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				isolateBlacksmithOwnership(t)
				claim := seedStopClaim(t, "tbx_phase_budget")
				unrelated := seedStopClaim(t, "tbx_other_budget")
				ctx, cancel := context.WithCancel(t.Context())
				if mode == "caller-deadline" {
					cancel()
					ctx, cancel = context.WithTimeout(t.Context(), 20*time.Second)
				}
				defer cancel()
				if mode == "caller-canceled" {
					go func() { time.Sleep(10 * time.Second); cancel() }()
				}
				if mode == "final-admission" {
					held, release, joined := make(chan struct{}), make(chan struct{}), make(chan error, 1)
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
				start := time.Now()
				var stdout, stderr bytes.Buffer
				var calls []string
				statuses, reads := 0, 0
				type replacementResult struct {
					claim core.LeaseClaim
					err   error
				}
				replacementDone := make(chan replacementResult, 1)
				denied, nativeFailure := errors.New("GitHub access denied"), errors.New("native stop failed")
				cfg := core.BaseConfig()
				cfg.Blacksmith.Org = "example-org"
				backend := newTestBlacksmithBackend(cfg, settlementRunner(func(gotCtx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
					deadline, ok := gotCtx.Deadline()
					wantDeadline := start.Add(30 * time.Second)
					if mode == "caller-deadline" {
						wantDeadline = start.Add(20 * time.Second)
					}
					if statuses >= 2 && reads >= 1 && mode != "late-denied" {
						wantDeadline = start.Add(40 * time.Second)
					}
					if !ok || deadline != wantDeadline {
						t.Fatalf("unexpected phase deadline: %s want %s", deadline, wantDeadline)
					}
					if req.Name == "gh" {
						calls = append(calls, "github")
						reads++
						result, ok := testCompletedGitHubRead(req)
						if !ok {
							t.Fatalf("unexpected GitHub mutation/request: %v", req.Args)
						}
						if reads == 1 {
							<-gotCtx.Done()
							if mode == "replacement" {
								go func() {
									replacement := claim
									replacement.RepoRoot = t.TempDir()
									updated, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, replacement)
									replacementDone <- replacementResult{updated, err}
								}()
								// The writer owns the mutation mutex while the current
								// shared fence still blocks its file lock. It publishes
								// before finalization can acquire that same mutex.
								synctest.Wait()
							}
							if mode == "late-denied" {
								return core.LocalCommandResult{ExitCode: 7}, denied
							}
							return core.LocalCommandResult{ExitCode: -1}, gotCtx.Err()
						}
						if mode == "final-canceled" {
							cancel()
						}
						if mode == "final-deadline" {
							<-gotCtx.Done()
						}
						if err := waitCleanupBudget(gotCtx, 300*time.Millisecond); err != nil {
							return core.LocalCommandResult{ExitCode: -1}, err
						}
						if mode == "final-denied" {
							return core.LocalCommandResult{ExitCode: 7}, denied
						}
						if mode == "final-pending" {
							result.Stdout = strings.Replace(result.Stdout, "completed", "in_progress", 1)
						}
						return result, nil
					}
					args := req.Args
					if len(args) > 2 && args[0] == "--org" {
						args = args[2:]
					}
					if req.Name != "blacksmith" || len(args) < 2 {
						t.Fatalf("unexpected request: %+v", req)
					}
					calls = append(calls, args[1])
					switch args[1] {
					case "stop":
						if err := waitCleanupBudget(gotCtx, 29800*time.Millisecond); err != nil {
							return core.LocalCommandResult{ExitCode: -1}, err
						}
						result := core.LocalCommandResult{Stdout: "native stopped\n", Stderr: "native stop note\n"}
						if mode == "final-denied" || mode == "success-native-error" {
							result.ExitCode = 7
							return result, nativeFailure
						}
						return result, nil
					case "status":
						statuses++
						state := "completed"
						if statuses == 1 || statuses == 3 && mode == "final-nonterminal" {
							state = "ready"
						}
						output := nativeStopStatus(claim.LeaseID, state, "")
						if statuses == 3 && mode == "final-changed-url" {
							output = strings.ReplaceAll(output, "123456789", "987654321")
						}
						return core.LocalCommandResult{Stdout: output}, nil
					default:
						t.Fatalf("unexpected native operation: %v", args)
						return core.LocalCommandResult{}, nil
					}
				}))
				backend.rt.Stdout, backend.rt.Stderr = &stdout, &stderr
				err := backend.Stop(ctx, core.StopRequest{ID: claim.LeaseID})
				var replacement replacementResult
				if mode == "replacement" {
					// Join before any fatal assertion: synctest stops advancing time
					// when its main goroutine exits, even on the expected red baseline.
					replacement = <-replacementDone
				}
				success := mode == "success" || mode == "success-native-error"
				if (err == nil) != success {
					t.Fatalf("cleanup outcome: %v", err)
				}
				want := []string{"status", "stop", "status", "github", "status", "github"}
				elapsed := 30300 * time.Millisecond
				switch mode {
				case "caller-deadline":
					want, elapsed = want[:2], 20*time.Second
				case "caller-canceled":
					want, elapsed = want[:2], 10*time.Second
				case "late-denied":
					want, elapsed = want[:4], 30*time.Second
				case "replacement":
					want, elapsed = want[:4], time.Since(start)
					if elapsed < 30*time.Second || elapsed >= 40*time.Second || !strings.Contains(err.Error(), "claim changed") {
						t.Fatalf("replacement did not fence recovery: elapsed=%s err=%v", elapsed, err)
					}
				case "final-changed-url", "final-nonterminal":
					want, elapsed = want[:5], 30*time.Second
				case "final-canceled":
					elapsed = 30 * time.Second
				case "final-deadline":
					elapsed = 40 * time.Second
				case "final-admission":
					want, elapsed = want[:4], 40*time.Second
				}
				if !reflect.DeepEqual(calls, want) || time.Since(start) != elapsed {
					t.Fatalf("order/budget: calls=%v elapsed=%s want=%v/%s", calls, time.Since(start), want, elapsed)
				}
				if mode == "late-denied" && !errors.Is(err, denied) {
					t.Fatal("ordinary request error lost")
				}
				if mode == "final-denied" && (!errors.Is(err, denied) || !errors.Is(err, nativeFailure)) {
					t.Fatal("earlier native or final verification failure lost")
				}
				if mode == "replacement" {
					if replacement.err != nil || replacement.claim.RepoRoot == claim.RepoRoot || replacement.claim.Revision == claim.Revision {
						t.Fatalf("replacement was not published: %+v", replacement)
					}
					assertStopState(t, replacement.claim, true)
				} else {
					assertStopState(t, claim, !success)
				}
				assertStopState(t, unrelated, true)
				wantStderr := "native stop note\n"
				if mode == "success-native-error" {
					wantStderr += "blacksmith cleanup reconciled lease=" + claim.LeaseID + " state=completed: stop failed; native status and exact GitHub run confirmed completion conclusion=cancelled\n"
				}
				if mode != "caller-deadline" && mode != "caller-canceled" && (stdout.String() != "native stopped\n" || stderr.String() != wantStderr) {
					t.Fatalf("stop output duplicated/lost: stdout=%q stderr=%q", stdout.String(), stderr.String())
				}
			})
		})
	}
}

func TestBlacksmithRunCleanupPhaseBudget(t *testing.T) {
	for _, code := range []int{0, 7} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				isolateBlacksmithOwnership(t)
				repo := t.TempDir()
				t.Chdir(repo)
				const id = "tbx_run_budget"
				runs, stops, reads := 0, 0, 0
				var stdout, stderr bytes.Buffer
				commandErr := errors.New("native command failed")
				runner := &blacksmithFuncRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
					switch req.Args[1] {
					case "warmup":
						return core.LocalCommandResult{Stdout: id + "\n"}, nil
					case "run":
						runs++
						if code != 0 {
							return core.LocalCommandResult{ExitCode: code}, commandErr
						}
					case "stop":
						stops++
						return core.LocalCommandResult{Stdout: "native stopped\n"}, nil
					}
					return core.LocalCommandResult{}, nil
				}}
				var cleanupStart time.Time
				runner.onRequest = func(ctx context.Context, req core.LocalCommandRequest) {
					if len(req.Args) > 1 && req.Args[1] == "stop" {
						cleanupStart = time.Now()
						if err := waitCleanupBudget(ctx, 29800*time.Millisecond); err != nil {
							t.Fatal(err)
						}
					}
				}
				cfg := core.BaseConfig()
				cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
				backend := newTestBlacksmithBackend(cfg, settlementRunner(func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
					if req.Name != "gh" {
						return runner.Run(ctx, req)
					}
					reads++
					if reads == 1 {
						<-ctx.Done()
						return core.LocalCommandResult{ExitCode: -1}, ctx.Err()
					}
					if err := waitCleanupBudget(ctx, 300*time.Millisecond); err != nil {
						return core.LocalCommandResult{ExitCode: -1}, err
					}
					result, ok := testCompletedGitHubRead(req)
					if !ok {
						t.Fatalf("unexpected GitHub request: %v", req.Args)
					}
					return result, nil
				}))
				backend.rt.Stdout, backend.rt.Stderr = &stdout, &stderr
				result, err := backend.Run(t.Context(), core.RunRequest{Repo: core.Repo{Root: repo}, Command: []string{"test-runner"}})
				if core.ExitCodeForError(err, 0) != code || result.ExitCode != code || code != 0 && !errors.Is(err, commandErr) {
					t.Fatalf("primary outcome changed: result=%+v err=%v", result, err)
				}
				if runs != 1 || stops != 1 || reads != 2 || time.Since(cleanupStart) != 30300*time.Millisecond {
					t.Fatalf("replay/order/budget: runs=%d stops=%d reads=%d elapsed=%s", runs, stops, reads, time.Since(cleanupStart))
				}
				if result.Session == nil || result.Session.Kept || strings.Count(stdout.String(), "native stopped\n") != 1 || strings.Contains(stderr.String(), "cleanup failed") {
					t.Fatalf("cleanup session/output: session=%+v stdout=%q stderr=%q", result.Session, stdout.String(), stderr.String())
				}
				claim, err := core.ReadLeaseClaim(id)
				key, keyErr := core.TestboxKeyPath(id)
				if err != nil || keyErr != nil {
					t.Fatalf("custody paths: %v %v", err, keyErr)
				}
				if _, err := os.Stat(key); claim.LeaseID != "" || !os.IsNotExist(err) {
					t.Fatalf("claim/key retained: %+v %v", claim, err)
				}
			})
		})
	}
}

func TestBlacksmithRollbackDeadlineRetainsPartialCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		isolateBlacksmithOwnership(t)
		const id = "tbx_rollback_budget"
		key, err := core.TestboxKeyPath(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(key, []byte("rollback-key"), 0o600); err != nil {
			t.Fatal(err)
		}
		statuses, reads := 0, 0
		backend := newTestBlacksmithBackend(core.BaseConfig(), settlementRunner(func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
			if req.Name == "gh" {
				if _, ok := testCompletedGitHubRead(req); !ok {
					t.Fatal("rollback attempted GitHub mutation")
				}
				reads++
				<-ctx.Done()
				return core.LocalCommandResult{ExitCode: -1}, ctx.Err()
			}
			if len(req.Args) > 3 && req.Args[0] == "--org" {
				req.Args = req.Args[2:]
			}
			if len(req.Args) < 2 || req.Args[1] != "status" {
				t.Fatalf("rollback replayed stop: %v", req.Args)
			}
			statuses++
			return core.LocalCommandResult{Stdout: nativeStopStatus(id, "completed", "")}, nil
		}))
		backend.route = &blacksmithRoute{API: blacksmithDefaultAPI, Org: "example-org"}
		var stderr bytes.Buffer
		backend.rt.Stderr = &stderr
		start := time.Now()
		backend.rollbackTestbox(id, id, t.TempDir())
		if data, err := os.ReadFile(key); err != nil || string(data) != "rollback-key" {
			t.Fatalf("rollback released uncertain key: %q %v", data, err)
		}
		claim, err := core.ReadLeaseClaim(id)
		if err != nil || claim.LeaseID != "" || statuses != 2 || reads != 1 || time.Since(start) != 30*time.Second || !strings.Contains(stderr.String(), "acquisition cleanup unconfirmed") {
			t.Fatalf("rollback gained claimed recovery: claim=%+v statuses=%d reads=%d elapsed=%s err=%v stderr=%s", claim, statuses, reads, time.Since(start), err, stderr.String())
		}
	})
}
