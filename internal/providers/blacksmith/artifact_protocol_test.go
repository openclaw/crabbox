//go:build darwin || linux

package blacksmith

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

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
							onRequest: func(runCtx context.Context, req LocalCommandRequest) { nativeCtx = runCtx },
							fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
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
								return LocalCommandResult{ExitCode: 1}, nativeCtx.Err()
							},
						}
						backend := newTestBlacksmithBackend(baseConfig(), runner)
						backend.rt.Stdout, backend.rt.Stderr = &stdout, &stderr
						req := RunRequest{ID: id, Repo: Repo{Root: repo}, Command: []string{"synthetic workload"}, ArtifactGlobs: []string{"report"}}
						wantCode := workloadCode
						if wantCode == 0 {
							wantCode = 1 // Preserve the native cancellation result after exit 0.
						}
						if boundary == "helper" {
							code, ended, artifacts, err := backend.runArtifactTestbox(ctx, req, id, nil, nil, nil, time.Second)
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
