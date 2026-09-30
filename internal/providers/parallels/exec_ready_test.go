package parallels

import (
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

func TestParallelsPrepareGuestExecRecovery(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, tc := range []struct {
			name, failure string
			failStep      int
			wantSteps     []string
			wantErr       bool
		}{
			{name: "probe", failure: "PrlJob_GetResult: Invalid argument", failStep: 0, wantSteps: []string{"probe", "probe", "key", "prep"}},
			{name: "key", failure: "PrlJob_GetResult: Invalid argument", failStep: 1, wantSteps: []string{"probe", "key", "probe", "key", "prep"}},
			{name: "prep", failure: "PrlJob_GetRetCode: Invalid argument", failStep: 2, wantSteps: []string{"probe", "key", "prep", "probe", "prep"}},
			{name: "terminal key", failure: "permission denied", failStep: 1, wantSteps: []string{"probe", "key"}, wantErr: true},
			{name: "terminal prep", failure: "node download failed", failStep: 2, wantSteps: []string{"probe", "key", "prep"}, wantErr: true},
		} {
			name := tc.name
			if remote {
				name += " remote"
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					cfg := core.BaseConfig()
					cfg.TargetOS = core.TargetMacOS
					cfg.SSHUser = "alice"
					cfg.Parallels.StartupTimeout = time.Minute
					if remote {
						cfg.Parallels.Host = "mac.example"
						cfg.Parallels.HostUser = "build"
					}
					var steps []string
					runner := execReadyRunner(func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
						if remote && !strings.Contains(strings.Join(req.Args, " "), "build@mac.example") {
							t.Fatalf("not routed to selected host: %v", req.Args)
						}
						step := "probe"
						if req.Stdin != nil {
							data, _ := io.ReadAll(req.Stdin)
							step = "prep"
							if strings.Contains(string(data), "authorized_keys") {
								step = "key"
							}
						}
						steps = append(steps, step)
						if len(steps)-1 == tc.failStep {
							return core.LocalCommandResult{Stderr: tc.failure}, errors.New("exit status 255")
						}
						return core.LocalCommandResult{}, nil
					})
					backend := NewBackend(Provider{}.Spec(), cfg, core.Runtime{Exec: runner, Stderr: io.Discard}).(*leaseBackend)
					err := backend.prepareGuest(context.Background(), core.NewParallelsClient(cfg, runner), "vm", core.ParallelsVM{IP: "10.211.55.8", IPSource: "tools"}, cfg, "ssh-ed25519 fixture")
					if (err != nil) != tc.wantErr {
						t.Fatalf("err=%v", err)
					}
					if strings.Join(steps, ",") != strings.Join(tc.wantSteps, ",") {
						t.Fatalf("steps=%v want=%v", steps, tc.wantSteps)
					}
				})
			})
		}
	}
}

type execReadyRunner func(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error)

func (f execReadyRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	return f(ctx, req)
}

func TestParallelsBootstrapWaitPrivacyHint(t *testing.T) {
	for _, tc := range []struct {
		hostOS, message string
		hint            bool
	}{
		{"darwin", "bootstrap ports=22:no-route-to-host", true},
		{"darwin", "dial tcp: no route to host", true},
		{"linux", "bootstrap ports=22:no-route-to-host", false},
		{"darwin", "bootstrap ports=22:closed", false},
		{"darwin", "guest readiness failed", false},
	} {
		original := core.Exit(5, "%s", tc.message)
		err := parallelsBootstrapWaitError(original, tc.hostOS)
		if strings.Contains(err.Error(), "Local Network") != tc.hint || !errors.Is(err, original) || core.ExitCodeForError(err, 1) != 5 {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}

func TestParallelsPrepareGuestRetriesBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := core.BaseConfig()
		cfg.TargetOS = core.TargetMacOS
		cfg.SSHUser = "alice"
		cfg.Parallels.StartupTimeout = 7 * time.Second
		start := time.Now()
		payloads := 0
		runner := execReadyRunner(func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
			if req.Stdin != nil {
				payloads++
				return core.LocalCommandResult{Stderr: "PrlJob_GetResult: Invalid argument"}, errors.New("exit status 255")
			}
			return core.LocalCommandResult{}, nil
		})
		backend := NewBackend(Provider{}.Spec(), cfg, core.Runtime{Exec: runner, Stderr: io.Discard}).(*leaseBackend)
		err := backend.prepareGuest(context.Background(), core.NewParallelsClient(cfg, runner), "vm", core.ParallelsVM{IPSource: "tools"}, cfg, "ssh-ed25519 fixture")
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 7*time.Second || payloads != 2 {
			t.Fatalf("err=%v elapsed=%s payloads=%d", err, time.Since(start), payloads)
		}
	})
}

func TestParallelsAcquisitionFailureRetention(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		for _, failAt := range []int{1, 2} {
			t.Run(fmt.Sprintf("fixed=%t payload=%d", fixed, failAt), func(t *testing.T) {
				backend, runner, req := fixedParallelsFixture(t)
				if !fixed {
					req.RequestedLeaseID = ""
				}
				payloads := 0
				backend.RT.Exec = execReadyRunner(func(ctx context.Context, command core.LocalCommandRequest) (core.LocalCommandResult, error) {
					if command.Name == "prlctl" && len(command.Args) > 0 && command.Args[0] == "exec" && command.Stdin != nil {
						payloads++
						if payloads == failAt {
							return core.LocalCommandResult{Stderr: "synthetic terminal preparation failure"}, errors.New("exit status 1")
						}
					}
					return runner.Run(ctx, command)
				})
				_, err := backend.Acquire(context.Background(), req)
				if err == nil || !strings.Contains(err.Error(), "synthetic terminal preparation failure") {
					t.Fatalf("err=%v", err)
				}
				if (len(runner.deleteCalls) == 0) != fixed {
					t.Fatalf("fixed=%t deleteCalls=%v", fixed, runner.deleteCalls)
				}
			})
		}
	}
}
