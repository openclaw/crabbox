package blacksmith

import (
	"context"
	"strings"
	"testing"
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
