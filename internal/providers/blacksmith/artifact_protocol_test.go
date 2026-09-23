//go:build darwin || linux

package blacksmith

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestBlacksmithRunSyncTimeoutOutcome(t *testing.T) {
	for _, tt := range []struct {
		name         string
		artifacts    bool
		trigger      string
		workloadCode int
		wantCode     int
		wantTimeout  bool
	}{
		{"normal-sync-timeout", false, "sync", -1, 124, true},
		{"artifact-sync-timeout", true, "sync", -1, 124, true},
		{"normal-command-124", false, "exit", 124, 124, false},
		{"artifact-command-124", true, "exit", 124, 124, false},
		{"artifact-workload-23-before-sync-timeout", true, "sync", 23, 23, false},
		{"artifact-workload-124-before-sync-timeout", true, "sync", 124, 124, false},
		{"normal-caller-canceled", false, "caller", -1, 1, false},
		{"artifact-caller-canceled", true, "caller", 0, 1, false},
		{"artifact-collection-canceled", true, "collection", 0, 1, false},
		{"artifact-workload-23-before-collection-cancel", true, "collection", 23, 23, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isolateArtifactOwnership(t)
			t.Setenv("CRABBOX_CONTROLLER_PROCESS_TREE_OWNED", "")
			t.Setenv("CRABBOX_BLACKSMITH_SYNC_TIMEOUT_MS", "50")
			repo := t.TempDir()
			t.Chdir(repo)
			const id = "tbx_sync_outcome"
			prepareBlacksmithGuestKey(t, id)
			testOwnedBlacksmithClaim(t, id, "sync-outcome", repo)
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var nativeCtx context.Context
				runs := 0
				runner := &blacksmithFuncRunner{
					onRequest: func(runCtx context.Context, _ core.LocalCommandRequest) { nativeCtx = runCtx },
					fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
						runs++
						if tt.trigger == "sync" {
							fmt.Fprintln(req.Stdout, "Syncing from repo root: /synthetic")
						}
						if tt.artifacts && tt.workloadCode >= 0 {
							start, exit, _ := testBlacksmithReceiptFrames(t, syntheticBlacksmithCommand(t, req), tt.workloadCode)
							fmt.Fprint(req.Stdout, start+"CRABBOX_PHASE:test\n"+exit)
						} else if tt.trigger != "sync" {
							fmt.Fprintln(req.Stdout, "CRABBOX_PHASE:test")
						}
						if tt.trigger == "exit" {
							return core.LocalCommandResult{ExitCode: 124}, errors.New("exit status 124")
						}
						if tt.trigger == "caller" {
							cancel()
						}
						// The real guard or collection timer cancels this native call;
						// synctest advances time without running a shell or a lease.
						<-nativeCtx.Done()
						return core.LocalCommandResult{ExitCode: 1}, nativeCtx.Err()
					},
				}
				var stderr bytes.Buffer
				backend := newTestBlacksmithBackend(core.BaseConfig(), runner)
				backend.rt.Stderr = &stderr
				req := core.RunRequest{ID: id, Repo: core.Repo{Root: repo}, Command: []string{"synthetic workload"}, TimingJSON: true, EmitProof: "proof"}
				if tt.artifacts {
					req.ArtifactGlobs = []string{"report"}
				}
				result, err := backend.Run(ctx, req)
				result = core.FinalizeRunResult(result, err)
				wantStatus, wantKind := core.RunStatusFailed, core.RunErrorCommandExit
				wantStage, wantRetry := "test", "unknown"
				if tt.wantTimeout {
					wantStatus, wantKind = core.RunStatusTimedOut, core.RunErrorTimeout
					wantStage, wantRetry = "sync", "true"
				}
				if runs != 1 || result.ExitCode != tt.wantCode || core.ExitCodeForError(err, 0) != tt.wantCode {
					t.Fatalf("runs=%d result=%+v err=%v", runs, result, err)
				}
				if result.Status != wantStatus || result.ErrorKind != wantKind {
					t.Errorf("public outcome=%s/%s, want %s/%s", result.Status, result.ErrorKind, wantStatus, wantKind)
				}
				if tt.trigger != "exit" && nativeCtx.Err() == nil {
					t.Fatal("native runner was not canceled")
				}
				if (ctx.Err() != nil) != (tt.trigger == "caller") {
					t.Fatalf("wrong cancellation owner: %v", ctx.Err())
				}
				checkReport := func(label string, data []byte) {
					t.Helper()
					var report core.TimingReport
					if err := json.Unmarshal(data, &report); err != nil {
						t.Fatalf("%s: %v", label, err)
					}
					if report.ExitCode != tt.wantCode || report.RunStatus != wantStatus || report.ErrorKind != wantKind || report.BlockedStage != wantStage || report.RetryLikely != wantRetry {
						t.Errorf("%s outcome=%d/%s/%s/%s/%s, want %d/%s/%s/%s/%s", label, report.ExitCode, report.RunStatus, report.ErrorKind, report.BlockedStage, report.RetryLikely, tt.wantCode, wantStatus, wantKind, wantStage, wantRetry)
					}
					if report.CommandMs != result.Command.Milliseconds() || report.TotalMs != result.Total.Milliseconds() || !report.SyncDelegated {
						t.Errorf("%s lost delegated timing: %+v", label, report)
					}
				}
				reports := 0
				for _, line := range strings.Split(stderr.String(), "\n") {
					if strings.HasPrefix(line, "{") {
						checkReport("timing", []byte(line))
						reports++
					}
				}
				if reports != 1 {
					t.Fatalf("timing reports=%d, want 1", reports)
				}
				proofs := 0
				for _, artifact := range result.Artifacts {
					if artifact.Kind == "artifact-glob" {
						t.Error("incomplete protocol published workload artifacts")
					}
					if artifact.Kind == "timing" {
						data, err := os.ReadFile(artifact.Path)
						if err != nil {
							t.Fatal(err)
						}
						checkReport("proof", data)
						proofs++
					}
				}
				if proofs != 1 {
					t.Fatalf("timing proofs=%d, want 1", proofs)
				}
				bundle := ""
				for _, field := range strings.Fields(stderr.String()) {
					if strings.HasPrefix(field, "local=.crabbox/captures/") {
						bundle = strings.TrimPrefix(field, "local=")
					}
				}
				if bundle == "" {
					t.Fatal("missing failure bundle")
				}
				checkReport("bundle", []byte(readBlacksmithArchive(t, bundle)["crabbox-artifacts/timings.json"]))
				summary := fmt.Sprintf("exit=%d blocked_stage=%s retry_likely=%s", tt.wantCode, wantStage, wantRetry)
				if !strings.Contains(stderr.String(), summary) {
					t.Errorf("summary missing %q", summary)
				}
				if tt.artifacts {
					assertArtifactTransferCalls(t, runner.calls, 0)
					assertNoBlacksmithArtifactPublication(t, repo, id)
				}
			})
		})
	}
}

func TestBlacksmithArtifactProtocolCancellation(t *testing.T) {
	for _, boundary := range []string{"helper", "public"} {
		for _, trigger := range []string{"caller", "collection-timer"} {
			for _, workloadCode := range []int{0, 23} {
				t.Run(fmt.Sprintf("%s/%s/%d", boundary, trigger, workloadCode), func(t *testing.T) {
					isolateArtifactOwnership(t)
					t.Setenv("CRABBOX_CONTROLLER_PROCESS_TREE_OWNED", "")
					repo := t.TempDir()
					t.Chdir(repo)
					const id = "tbx_protocol_cancel"
					prepareBlacksmithGuestKey(t, id)
					if boundary == "public" {
						testOwnedBlacksmithClaim(t, id, "jade-krill", repo)
					}
					synctest.Test(t, func(t *testing.T) {
						ctx, cancel := context.WithCancelCause(t.Context())
						defer cancel(nil)
						callerCause := errors.New("synthetic caller abort")
						var nativeCtx context.Context
						var observedCause error
						var stdout, stderr bytes.Buffer
						runs := 0
						const privatePayload = "synthetic-private-collection-payload"
						runner := &blacksmithFuncRunner{
							onRequest: func(runCtx context.Context, req core.LocalCommandRequest) { nativeCtx = runCtx },
							fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
								runs++
								start, workloadExit, _ := testBlacksmithReceiptFrames(t, syntheticBlacksmithCommand(t, req), workloadCode)
								_, _ = io.WriteString(req.Stdout, start+"ordinary workload output\n"+workloadExit+privatePayload)
								if trigger == "caller" {
									cancel(callerCause)
								}
								// The real collection timer advances under synctest; no native
								// process or wall-clock timeout is needed for this protocol edge.
								<-nativeCtx.Done()
								observedCause = context.Cause(nativeCtx)
								return core.LocalCommandResult{ExitCode: 1}, nativeCtx.Err()
							},
						}
						backend := newTestBlacksmithBackend(core.BaseConfig(), runner)
						backend.rt.Stdout, backend.rt.Stderr = &stdout, &stderr
						req := core.RunRequest{ID: id, Repo: core.Repo{Root: repo}, Command: []string{"synthetic workload"}, ArtifactGlobs: []string{"report"}}
						wantCode := workloadCode
						if wantCode == 0 {
							wantCode = 1 // Preserve the native cancellation result after exit 0.
						}
						if boundary == "helper" {
							outcome, ended, artifacts, err := backend.runArtifactTestbox(ctx, req, id, nil, nil, nil, time.Second)
							code := outcome.code
							if code != wantCode || ended.IsZero() || len(artifacts) != 0 {
								t.Errorf("protocol abort lost workload outcome: code=%d ended=%v artifacts=%v", code, ended, artifacts)
							}
							if observedCause == nil || !errors.Is(err, context.Canceled) || !errors.Is(err, observedCause) {
								t.Errorf("protocol abort lost cancellation classification or cause: %v", err)
							}
							if core.ExitCodeForError(err, 0) != 7 || strings.Contains(err.Error(), privatePayload) {
								t.Errorf("protocol error changed code or exposed payload: %v", err)
							}
						} else {
							result, err := backend.Run(ctx, req)
							finalized := core.FinalizeRunResult(result, err)
							if result.ExitCode != wantCode || core.ExitCodeForError(err, 0) != wantCode || finalized.Status != core.RunStatusFailed || finalized.ErrorKind != core.RunErrorCommandExit {
								t.Errorf("secondary cancellation changed public workload classification: result=%+v err=%v", finalized, err)
							}
							if observedCause == nil || !strings.Contains(stderr.String(), observedCause.Error()) {
								t.Errorf("public artifact diagnostic lost cancellation cause: %v", stderr.String())
							}
							for _, artifact := range result.Artifacts {
								if artifact.Kind == "artifact-glob" {
									t.Error("aborted protocol published an artifact")
								}
							}
						}
						if trigger == "caller" && observedCause != callerCause {
							t.Errorf("fixture did not observe the caller cause: %v", observedCause)
						}
						if trigger == "collection-timer" && (ctx.Err() != nil || observedCause == nil || observedCause.Error() != "artifact collection timed out") {
							t.Errorf("fixture did not isolate collection cancellation: parent=%v cause=%v", ctx.Err(), observedCause)
						}
						if runs != 1 || strings.Contains(stdout.String()+stderr.String(), privatePayload) || strings.Contains(stdout.String()+stderr.String(), "\x1e") {
							t.Errorf("protocol retried or exposed collection data: runs=%d", runs)
						}
						assertArtifactTransferCalls(t, runner.calls, 0)
						assertNoBlacksmithArtifactPublication(t, repo, id)
					})
				})
			}
		}
	}
}
