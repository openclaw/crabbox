package blacksmith

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestBlacksmithStatusUsesExactNativeSnapshot(t *testing.T) {
	for _, state := range []string{"queued", "hydrating", "ready", "running", "in_progress", "hydration_failed", "completed"} {
		t.Run(state, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			cfg := core.BaseConfig()
			cfg.Blacksmith.Org = "example-org"
			calls := 0
			backend := newTestBlacksmithBackend(cfg, ownershipRunner(func(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				calls++
				if req.Name != "blacksmith" || strings.Join(req.Args, " ") != "testbox status --id tbx_exact --api-url https://backend.blacksmith.sh --org example-org" {
					t.Fatalf("snapshot must use exact scoped status, not inventory: %+v", req)
				}
				return core.LocalCommandResult{Stdout: nativeNeverAssignedStatus("tbx_exact", state)}, nil
			}))
			view, err := backend.Status(t.Context(), core.StatusRequest{ID: "tbx_exact"})
			wantReady := state == "ready" || state == "running" || state == "in_progress"
			if err != nil || calls != 1 || view.ID != "tbx_exact" || view.State != state || view.Ready != wantReady || view.Labels["workflow"] != ".github/workflows/testbox.yml" {
				t.Fatalf("snapshot=%+v calls=%d err=%v", view, calls, err)
			}
			if state == "completed" || state == "hydration_failed" {
				_, err = backend.Status(t.Context(), core.StatusRequest{ID: "tbx_exact", Wait: true, WaitTimeout: time.Minute})
				var exit core.ExitError
				if !core.AsExitError(err, &exit) || exit.Code != 5 || calls != 2 {
					t.Fatalf("terminal wait did not fail immediately: calls=%d err=%v", calls, err)
				}
			}
		})
	}
}

func TestBlacksmithStatusRemoteSettlement(t *testing.T) {
	const runURL = "https://github.com/example-org/my-app/actions/runs/123456789"
	for _, mode := range []string{"ready", "running", "queued", "complete", "success", "failure", "pending", "missing", "invalid", "denied", "missing-gh", "wrong-id", "wrong-url", "unsupported", "no-conclusion", "changed", "disappeared", "changed-workflow", "changed-created", "active-again", "recheck-error"} {
		t.Run(mode, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			claim := seedStopClaim(t, "tbx_status")
			keyPath, err := core.TestboxKeyPath(claim.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(keyPath)
			if err != nil {
				t.Fatal(err)
			}
			cfg := core.BaseConfig()
			cfg.Blacksmith.Org = "example-org"
			reads, statuses := 0, 0
			backend := newTestBlacksmithBackend(cfg, settlementRunner(func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > blacksmithStatusReadTimeout {
					t.Fatal("unbounded status read")
				}
				if req.Name == "gh" {
					reads++
					want := []string{"api", "--hostname", "github.com", "--method", "GET", "repos/example-org/my-app/actions/runs/123456789", "--jq", "{id,html_url,status,conclusion}"}
					if !reflect.DeepEqual(req.Args, want) || req.MaxCapturedOutputBytes != 16384 {
						t.Fatalf("non-exact GitHub read: %+v", req)
					}
					if mode == "denied" {
						return core.LocalCommandResult{ExitCode: 4, Stderr: "sensitive diagnostic"}, nil
					}
					if mode == "missing-gh" {
						return core.LocalCommandResult{}, os.ErrNotExist
					}
					id, url, state, conclusion := "123456789", runURL, "completed", "cancelled"
					switch mode {
					case "success", "failure":
						conclusion = mode
					case "pending":
						state, conclusion = "in_progress", ""
					case "wrong-id":
						id = "987"
					case "wrong-url":
						url += "0"
					case "unsupported":
						state = "unknown"
					case "no-conclusion":
						conclusion = ""
					}
					return core.LocalCommandResult{Stdout: fmt.Sprintf(`{"id":%s,"html_url":%q,"status":%q,"conclusion":%q}`, id, url, state, conclusion)}, nil
				}
				if req.Name != "blacksmith" || strings.Join(req.Args, " ") != "testbox status --id tbx_status --api-url https://backend.blacksmith.sh --org example-org" {
					t.Fatalf("unexpected mutation or route: %+v", req)
				}
				statuses++
				state, url, workflow, created := "completed", runURL, ".github/workflows/testbox.yml", "2026-10-03T12:00:00Z"
				switch mode {
				case "ready", "running", "queued":
					state = mode
				case "missing":
					url = ""
				case "invalid":
					url = "https://github.com/example-org/../actions/runs/123456789"
				}
				if statuses > 1 {
					switch mode {
					case "changed":
						url += "0"
					case "disappeared":
						url = ""
					case "changed-workflow":
						workflow = "other.yml"
					case "changed-created":
						created = "2026-10-03T13:00:00Z"
					case "active-again":
						state = "ready"
					case "recheck-error":
						return core.LocalCommandResult{}, errors.New("failed read")
					}
				}
				return core.LocalCommandResult{Stdout: nativeStopStatusTable("tbx_status", state, "", workflow, "test", "main", created, url)}, nil
			}))
			view, err := backend.Status(t.Context(), core.StatusRequest{ID: "tbx_status"})
			want, wantReads := "unknown", 1
			switch mode {
			case "ready", "running", "queued":
				want, wantReads = "pending", 0
			case "missing", "invalid":
				wantReads = 0
			case "pending":
				want = "pending"
			case "complete", "success", "failure":
				want = "complete"
			}
			if err != nil || view.ProviderMetadata["remoteSettlement"] != want || view.CleanupStatus != "" || reads != wantReads || statuses > 2 {
				t.Fatalf("view=%+v err=%v reads=%d statuses=%d", view, err, reads, statuses)
			}
			if (view.ProviderMetadata["runConclusion"] != nil) != (want == "complete") {
				t.Fatalf("unsupported conclusion: %+v", view)
			}
			if mode != "missing" && mode != "invalid" && view.ProviderMetadata["runURL"] != runURL {
				t.Fatalf("lost association: %+v", view)
			}
			after, err := os.ReadFile(keyPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("status changed key")
			}
			current, err := core.ReadLeaseClaim(claim.LeaseID)
			if err != nil || !reflect.DeepEqual(claim, current) {
				t.Fatal("status changed claim")
			}
		})
	}
}

func TestBlacksmithStatusReadCancellationAndDeadline(t *testing.T) {
	for _, stage := range []string{"route", "native", "github", "recheck"} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancel=%t", stage, canceled), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					isolateBlacksmithOwnership(t)
					cfg := core.BaseConfig()
					if stage != "route" {
						cfg.Blacksmith.Org = "example-org"
					}
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					statuses := 0
					backend := newTestBlacksmithBackend(cfg, settlementRunner(func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
						if req.Name == "blacksmith" && req.Args[0] == "testbox" {
							statuses++
						}
						block := stage == "route" || stage == "native" || stage == "github" && req.Name == "gh" || stage == "recheck" && statuses == 2
						if block {
							if canceled {
								cancel()
							}
							<-ctx.Done()
							return core.LocalCommandResult{}, ctx.Err()
						}
						if result, ok := testCompletedGitHubRead(req); ok {
							return result, nil
						}
						return core.LocalCommandResult{Stdout: testBlacksmithStatus("tbx_status", "completed")}, nil
					}))
					start := time.Now()
					_, err := backend.Status(ctx, core.StatusRequest{ID: "tbx_status"})
					want := context.DeadlineExceeded
					if canceled {
						want = context.Canceled
					}
					if !errors.Is(err, want) || time.Since(start) > blacksmithStatusReadTimeout {
						t.Fatalf("unbounded/cancellation swallowed: %v duration=%s", err, time.Since(start))
					}
				})
			})
		}
	}
}

func TestBlacksmithStatusWaitTerminalSkipsGitHub(t *testing.T) {
	isolateBlacksmithOwnership(t)
	cfg := core.BaseConfig()
	cfg.Blacksmith.Org = "example-org"
	calls := 0
	backend := newTestBlacksmithBackend(cfg, settlementRunner(func(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		calls++
		if req.Name != "blacksmith" || req.Args[1] != "status" {
			t.Fatalf("readiness wait must not verify settlement: %+v", req)
		}
		return core.LocalCommandResult{Stdout: testBlacksmithStatus("tbx_status", "completed")}, nil
	}))
	_, err := backend.Status(t.Context(), core.StatusRequest{ID: "tbx_status", Wait: true, WaitTimeout: time.Minute})
	var exit core.ExitError
	if !core.AsExitError(err, &exit) || exit.Code != 5 || calls != 1 {
		t.Fatalf("changed terminal readiness: calls=%d err=%v", calls, err)
	}
}
