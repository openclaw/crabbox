package parallels

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// A guest whose DHCP never answers keeps its self-assigned address until the
// startup deadline. Acquisition replaces that clone once, as it did when the
// address reached the SSH wait.
func TestParallelsAcquireRetriesGuestStuckOnLinkLocalAddress(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	backend.Cfg.TargetOS = core.TargetWindows
	backend.Cfg.Parallels.StartupTimeout = 3 * time.Minute
	req.RequestedLeaseID = ""
	backend.RT.Exec = linkLocalRunner{runner}
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		_, err := backend.Acquire(context.Background(), req)
		if !core.IsBootstrapWaitError(err) || core.ExitCodeForError(err, 1) != 5 || !strings.Contains(err.Error(), "tools_ip=169.254.2.136 (link-local") {
			t.Fatalf("want a retryable link-local IP timeout, got %v", err)
		}
		if len(runner.cloneCalls) != 2 || len(runner.deleteCalls) != 2 || time.Since(started) != 6*time.Minute {
			t.Fatalf("clones=%v deletes=%v elapsed=%s, want two 3m attempts", runner.cloneCalls, runner.deleteCalls, time.Since(started))
		}
	})
}

type linkLocalRunner struct{ *parallelsFixedRunner }

func (r linkLocalRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	result, err := r.parallelsFixedRunner.Run(ctx, req)
	if req.Name == "prlctl" && len(req.Args) > 0 && req.Args[0] == "list" {
		result.Stdout = strings.ReplaceAll(result.Stdout, "10.211.55.9", "169.254.2.136")
	}
	return result, err
}
