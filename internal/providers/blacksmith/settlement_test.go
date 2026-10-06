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

// Unlike the native-only fixtures, this runner does not supply a successful
// GitHub response implicitly. Every read and mutation below is asserted.
type settlementRunner func(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error)

func (f settlementRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	return f(ctx, req)
}

func TestBlacksmithRollbackRetainsUnassociatedRecoveryKey(t *testing.T) {
	isolateBlacksmithOwnership(t)
	const pending = "tbx_pending_rollback"
	key, _, err := core.EnsureTestboxKey(pending)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	stops := 0
	backend := newTestBlacksmithBackend(core.BaseConfig(), settlementRunner(func(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if req.Name != "blacksmith" || testBlacksmithFlag(req.Args, "--id") != "tbx_rollback" || testBlacksmithFlag(req.Args, "--org") != "example-org" {
			t.Fatalf("unexpected rollback command: %+v", req)
		}
		if req.Args[1] == "stop" {
			stops++
			return core.LocalCommandResult{}, nil
		}
		if req.Args[1] != "status" {
			t.Fatalf("unexpected command: %+v", req)
		}
		state := "queued"
		if stops > 0 {
			state = "completed"
		}
		return core.LocalCommandResult{Stdout: nativeNeverAssignedStatus("tbx_rollback", state)}, nil
	}))
	backend.route = &blacksmithRoute{API: blacksmithDefaultAPI, Org: "example-org"}
	backend.rt.Stderr = &stderr
	backend.rollbackTestbox("tbx_rollback", pending, t.TempDir())
	if stops != 1 || !strings.Contains(stderr.String(), "acquisition cleanup unconfirmed") || !strings.Contains(stderr.String(), "GitHub settlement unresolved") || !strings.Contains(stderr.String(), pending) {
		t.Fatalf("rollback result: stops=%d stderr=%s", stops, stderr.String())
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("lost recovery key: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(key)); err != nil {
		t.Fatal(err)
	}
	claims, err := core.ListLeaseClaims()
	if err != nil || len(claims) != 0 {
		t.Fatalf("rollback invented ownership: %+v %v", claims, err)
	}
}

func TestBlacksmithStopRequiresExactGitHubSettlement(t *testing.T) {
	for _, mode := range []string{"cancelled", "success", "failure", "no-association", "late-association", "retry-late", "pending", "missing-gh", "denied", "nonzero-without-error", "wrong-id", "wrong-url", "unknown-status", "missing-conclusion", "invalid-url", "changed-association", "lost-association", "final-pending", "final-conclusion", "final-association", "native-error", "native-error-unsettled", "late-native-error-unsettled", "cancel-read"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				isolateBlacksmithOwnership(t)
				t.Setenv("GH_HOST", "unrelated.example")
				const id = "tbx_settlement"
				const runURL = "https://github.com/example-org/my-app/actions/runs/123456789"
				claim := seedStopClaim(t, id)
				other := seedStopClaim(t, "tbx_other")
				cfg := core.BaseConfig()
				cfg.Blacksmith.Org = "example-org"
				var stderr bytes.Buffer
				stops, reads, statuses := 0, 0, 0
				associated := mode != "no-association" && mode != "late-association" && mode != "retry-late" && mode != "late-native-error-unsettled"
				ctx, cancel := context.WithTimeout(t.Context(), 2500*time.Millisecond)
				defer cancel()
				backend := newTestBlacksmithBackend(cfg, settlementRunner(func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
					if req.Name == "gh" {
						reads++
						want := []string{"api", "--hostname", "github.com", "--method", "GET", "repos/example-org/my-app/actions/runs/123456789", "--jq", "{id,html_url,status,conclusion}"}
						if !reflect.DeepEqual(req.Args, want) || req.MaxCapturedOutputBytes <= 0 || req.MaxCapturedOutputBytes > 16384 {
							t.Fatalf("unbounded or non-exact GitHub read: %+v", req)
						}
						if _, ok := ctx.Deadline(); !ok {
							t.Fatal("GitHub read has no deadline")
						}
						switch mode {
						case "missing-gh":
							return core.LocalCommandResult{ExitCode: 1}, os.ErrNotExist
						case "denied":
							return core.LocalCommandResult{ExitCode: 4, Stderr: "synthetic inaccessible credential diagnostic"}, errors.New("read denied")
						case "nonzero-without-error":
							return core.LocalCommandResult{ExitCode: 4}, nil
						case "cancel-read":
							cancel()
						}
						status, conclusion, url, runID := "completed", "cancelled", runURL, "123456789"
						switch mode {
						case "success", "failure":
							conclusion = mode
						case "wrong-id":
							runID = "987"
						case "wrong-url":
							url = strings.ReplaceAll(runURL, "my-app", "other-app")
						case "unknown-status":
							status = "unknown"
						case "final-conclusion":
							if reads > 1 {
								conclusion = "success"
							}
						case "missing-conclusion":
							conclusion = ""
						case "pending", "native-error-unsettled", "late-native-error-unsettled":
							status, conclusion = "pending", ""
						case "late-association", "retry-late":
							if stops < 2 {
								status, conclusion = "queued", ""
							}
						case "final-pending":
							if reads > 1 {
								status, conclusion = "in_progress", ""
							}
						}
						return core.LocalCommandResult{Stdout: fmt.Sprintf(`{"id":%s,"html_url":%q,"status":%q,"conclusion":%q}`, runID, url, status, conclusion)}, nil
					}
					if req.Name != "blacksmith" || testBlacksmithFlag(req.Args, "--id") != id || testBlacksmithFlag(req.Args, "--org") != "example-org" || testBlacksmithFlag(req.Args, "--api-url") != blacksmithDefaultAPI {
						t.Fatalf("unowned native access: %+v", req)
					}
					if strings.Contains(strings.Join(req.Args, " "), "testbox stop") {
						stops++
						if mode == "late-association" || mode == "late-native-error-unsettled" {
							associated = true
						}
						if mode == "late-native-error-unsettled" {
							if stops == 1 {
								return core.LocalCommandResult{ExitCode: 9, Stderr: "first native diagnostic\n"}, errors.New("native stop failed")
							}
							return core.LocalCommandResult{Stderr: "second native diagnostic\n"}, nil
						}
						if strings.HasPrefix(mode, "native-error") || mode == "final-conclusion" {
							return core.LocalCommandResult{ExitCode: 9}, errors.New("native stop failed")
						}
						return core.LocalCommandResult{}, nil
					}
					if !strings.Contains(strings.Join(req.Args, " "), "testbox status") {
						t.Fatalf("unexpected command: %+v", req)
					}
					statuses++
					state, url := "queued", ""
					if associated {
						url = runURL
					}
					if stops > 0 {
						state = "completed"
					}
					if mode == "invalid-url" {
						url = "https://github.com/example-org/../actions/runs/123456789"
					}
					if mode == "changed-association" && statuses > 1 || mode == "final-association" && statuses > 2 {
						url = strings.ReplaceAll(runURL, "123456789", "987654321")
					}
					if mode == "lost-association" && statuses > 1 {
						url = ""
					}
					return core.LocalCommandResult{Stdout: nativeStopStatusTable(id, state, "", claim.Labels["workflow"], claim.Labels["job"], claim.Labels["ref"], "2026-10-03T12:00:00Z", url)}, nil
				}))
				backend.rt.Stderr = &stderr
				err := backend.Stop(ctx, core.StopRequest{ID: id})
				if mode == "retry-late" {
					if err == nil || reads != 0 || stops != 1 {
						t.Fatalf("missing association did not retain: err=%v reads=%d stops=%d", err, reads, stops)
					}
					assertStopState(t, claim, true)
					associated = true
					err = backend.Stop(ctx, core.StopRequest{ID: id})
				}
				wantSuccess := mode == "cancelled" || mode == "success" || mode == "failure" || mode == "late-association" || mode == "retry-late" || mode == "native-error" || mode == "final-conclusion"
				if (err == nil) != wantSuccess {
					t.Fatalf("err=%v stops=%d reads=%d statuses=%d", err, stops, reads, statuses)
				}
				assertStopState(t, claim, !wantSuccess)
				assertStopState(t, other, true)
				wantStops := 1
				if mode == "invalid-url" {
					wantStops = 0
				}
				if mode == "late-association" || mode == "retry-late" || mode == "late-native-error-unsettled" {
					wantStops = 2
				}
				if stops != wantStops {
					t.Fatalf("native stops=%d want=%d", stops, wantStops)
				}
				if strings.Contains(stderr.String(), "synthetic inaccessible credential") {
					t.Fatal("GitHub diagnostics leaked")
				}
				if mode == "native-error-unsettled" || mode == "late-native-error-unsettled" {
					var exit core.ExitError
					if !core.AsExitError(err, &exit) || exit.Code != 9 {
						t.Fatalf("native error lost: %v", err)
					}
				}
				if mode == "late-native-error-unsettled" && stderr.String() != "first native diagnostic\nsecond native diagnostic\n" {
					t.Fatalf("native retry lost diagnostics: %q", stderr.String())
				}
				if mode == "final-conclusion" && (!strings.Contains(stderr.String(), "conclusion=success") || strings.Contains(stderr.String(), "conclusion=cancelled")) {
					t.Fatalf("stale conclusion: %s", stderr.String())
				}
				if mode == "cancel-read" && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			})
		})
	}
}
