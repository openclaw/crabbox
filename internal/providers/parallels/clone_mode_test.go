package parallels

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestParallelsAcquireResolvesCloneModeBeforeProvisioning(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "generated ID", true: "fixed ID"}[fixed], func(t *testing.T) {
			backend, runner, req := fixedParallelsFixture(t)
			backend.Cfg.TargetOS = core.TargetMacOS
			backend.Cfg.Parallels.CloneMode = ""
			backend.RT.Exec = &parallelsAppleSiliconRunner{CommandRunner: runner}
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
			if !strings.Contains(output.String(), "clone_mode=full") || strings.Contains(output.String(), "warning:") || submissions != 1 {
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

func TestParallelsAcquireWarnsAndSubmitsLinked(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "generated ID", true: "fixed ID"}[fixed], func(t *testing.T) {
			backend, runner, req := fixedParallelsFixture(t)
			backend.Cfg.TargetOS = core.TargetMacOS
			backend.Cfg.Parallels.CloneMode = "linked"
			backend.Cfg.Parallels.SourceSnapshotID = "snap"
			hostRunner := &parallelsAppleSiliconRunner{CommandRunner: runner}
			backend.RT.Exec = hostRunner
			var output bytes.Buffer
			backend.RT.Stderr = &output
			runner.cloneErr = errors.New("synthetic clone stop")
			runner.cloneCommit = fixed
			runner.beforeClone = func() {
				if strings.Count(output.String(), "warning: linked clones") != 1 {
					t.Fatalf("expected one warning before cloning: %s", output.String())
				}
			}
			if !fixed {
				req.RequestedLeaseID = ""
			}
			_, err := backend.Acquire(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), "synthetic clone stop") {
				t.Fatalf("acquisition must reach linked clone submission: %v", err)
			}
			if len(hostRunner.cloneArgs) != 1 || !slices.Contains(hostRunner.cloneArgs[0], "--linked") || !slices.Contains(hostRunner.cloneArgs[0], "snap") {
				t.Fatalf("expected linked clone with snapshot: %v", hostRunner.cloneArgs)
			}
			if !strings.Contains(output.String(), "clone_mode=linked") || !strings.Contains(output.String(), "--parallels-source-snapshot= --parallels-source-snapshot-id=") {
				t.Fatalf("missing linked mode or recovery guidance: %s", output.String())
			}
			if fixed {
				output.Reset()
				// Replay of an explicit mode needs neither a host architecture
				// probe nor another clone or its warning.
				backend.RT.Exec = runner
				_, err = backend.Acquire(context.Background(), req)
				if err != nil || len(runner.cloneCalls) != 1 || strings.Contains(output.String(), "warning: linked clones") {
					t.Fatalf("linked replay changed intent or resubmitted: err=%v output=%s clones=%v", err, output.String(), runner.cloneCalls)
				}
				backend.Cfg.Parallels.CloneMode = "full"
				_, err = backend.Acquire(context.Background(), req)
				if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") || len(runner.cloneCalls) != 1 {
					t.Fatalf("changed clone mode must conflict with fixed intent: err=%v clones=%v", err, runner.cloneCalls)
				}
			}
		})
	}
}

func TestParallelsAcquireLinkedWarningOnceAcrossRetries(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	backend.Cfg.TargetOS = core.TargetMacOS
	backend.Cfg.Parallels.CloneMode = "linked"
	backend.Cfg.Parallels.SourceSnapshotID = "snap"
	req.RequestedLeaseID = ""
	hostRunner := &parallelsAppleSiliconRunner{CommandRunner: runner}
	backend.RT.Exec = hostRunner
	var output bytes.Buffer
	backend.RT.Stderr = &output
	waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return core.Exit(5, "timed out waiting for SSH: synthetic bootstrap stop")
	}
	runner.beforeClone = func() {
		if strings.Count(output.String(), "warning: linked clones") != 1 {
			t.Fatalf("expected one warning before each attempt: %s", output.String())
		}
	}
	_, err := backend.Acquire(context.Background(), req)
	if err == nil || !core.IsBootstrapWaitError(err) || len(hostRunner.cloneArgs) != 2 || strings.Count(output.String(), "warning: linked clones") != 1 {
		t.Fatalf("expected two attempts and one linked-clone warning: err=%v clones=%v output=%s", err, hostRunner.cloneArgs, output.String())
	}
}

type parallelsAppleSiliconRunner struct {
	core.CommandRunner
	cloneArgs [][]string
}

func (r *parallelsAppleSiliconRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if req.Name == "/usr/sbin/sysctl" && strings.Join(req.Args, " ") == "-n -i hw.optional.arm64" {
		return core.LocalCommandResult{Stdout: "1\n"}, nil
	}
	if req.Name == "prlctl" && len(req.Args) > 0 && req.Args[0] == "clone" {
		r.cloneArgs = append(r.cloneArgs, slices.Clone(req.Args))
	}
	return r.CommandRunner.Run(ctx, req)
}
