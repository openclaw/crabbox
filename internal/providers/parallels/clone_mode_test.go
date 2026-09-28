package parallels

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestParallelsAcquireResolvesCloneModeBeforeProvisioning(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "generated ID", true: "fixed ID"}[fixed], func(t *testing.T) {
			backend, runner, req := fixedParallelsFixture(t)
			backend.Cfg.TargetOS = core.TargetMacOS
			backend.Cfg.Parallels.CloneMode = ""
			backend.RT.Exec = parallelsAppleSiliconRunner{runner}
			var output bytes.Buffer
			backend.RT.Stderr = &output
			runner.cloneErr = errors.New("synthetic clone stop")
			runner.cloneCommit = fixed
			submissions := 0
			runner.beforeClone = func() { submissions++ }
			if !fixed {
				req.RequestedLeaseID = ""
			}
			_, err := backend.Acquire(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), "synthetic clone stop") {
				t.Fatalf("acquisition must reach full clone submission: %v", err)
			}
			if !strings.Contains(output.String(), "clone_mode=full") || submissions != 1 {
				t.Fatalf("output=%s submissions=%d", output.String(), submissions)
			}
			if fixed {
				// Resolving the default must happen before fingerprinting, so
				// selecting the same mode explicitly is the same create intent.
				backend.Cfg.Parallels.CloneMode = "full"
				_, err = backend.Acquire(context.Background(), req)
				if err != nil || submissions != 1 {
					t.Fatalf("replay changed identity or resubmitted: err=%v submissions=%d", err, submissions)
				}
			}
		})
	}
}

func TestParallelsAcquireRejectsLinkedBeforeProvisioning(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "generated ID", true: "fixed ID"}[fixed], func(t *testing.T) {
			backend, runner, req := fixedParallelsFixture(t)
			backend.Cfg.TargetOS = core.TargetMacOS
			backend.Cfg.Parallels.CloneMode = "linked"
			backend.Cfg.Parallels.SourceSnapshotID = "snap"
			backend.RT.Exec = parallelsAppleSiliconRunner{runner}
			if !fixed {
				req.RequestedLeaseID = ""
			}
			_, err := backend.Acquire(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), "--parallels-clone-mode full") || len(runner.cloneCalls)+len(runner.startCalls)+len(runner.hostDirCalls) != 0 {
				t.Fatalf("linked acquisition must fail before provisioning: %v", err)
			}
		})
	}
}

type parallelsAppleSiliconRunner struct{ core.CommandRunner }

func (r parallelsAppleSiliconRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if req.Name == "/usr/sbin/sysctl" && strings.Join(req.Args, " ") == "-n hw.optional.arm64" {
		return core.LocalCommandResult{Stdout: "1\n"}, nil
	}
	return r.CommandRunner.Run(ctx, req)
}
