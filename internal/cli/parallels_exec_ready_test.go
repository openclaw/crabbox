package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestParallelsGuestExecReadinessBounds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		replies     []string
		block       bool
		wantCalls   int
		wantElapsed time.Duration
		wantErr     string
	}{
		{name: "Tools result recovers", replies: []string{"PrlJob_GetResult: Invalid argument", ""}, wantCalls: 2, wantElapsed: 5 * time.Second},
		{name: "Tools return code recovers", replies: []string{"PrlJob_GetRetCode: Invalid argument", ""}, wantCalls: 2, wantElapsed: 5 * time.Second},
		{name: "terminal failure", replies: []string{"permission denied"}, wantCalls: 1, wantErr: "permission denied"},
		{name: "blocked exec", block: true, wantCalls: 1, wantElapsed: 7 * time.Second, wantErr: "timed out waiting"},
		{name: "startup exhausted", replies: []string{"PrlJob_GetResult: Invalid argument"}, wantCalls: 2, wantElapsed: 7 * time.Second, wantErr: "timed out waiting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				calls := 0
				runner := parallelsExecReadyRunner(func(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
					calls++
					if tc.block {
						<-ctx.Done()
						return LocalCommandResult{}, ctx.Err()
					}
					reply := tc.replies[min(calls-1, len(tc.replies)-1)]
					if reply != "" {
						return LocalCommandResult{Stderr: reply}, errors.New("exit status 255")
					}
					return LocalCommandResult{}, nil
				})
				cfg := Config{TargetOS: targetMacOS}
				// The parent bounds the old implementation too, making deadline regressions finite.
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				err := NewParallelsClient(cfg, runner).WaitForGuestExec(ctx, "vm", cfg, 7*time.Second)
				if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
					t.Fatalf("err=%v want=%q", err, tc.wantErr)
				}
				if calls != tc.wantCalls || time.Since(start) != tc.wantElapsed {
					t.Fatalf("calls=%d elapsed=%s; want %d %s", calls, time.Since(start), tc.wantCalls, tc.wantElapsed)
				}
			})
		})
	}
}

func TestParallelsGuestExecCanceledBeforeProbe(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("caller stopped")
	cancel(cause)
	runner := parallelsExecReadyRunner(func(context.Context, LocalCommandRequest) (LocalCommandResult, error) {
		t.Fatal("probe after cancellation")
		return LocalCommandResult{}, nil
	})
	cfg := Config{TargetOS: targetMacOS}
	if err := NewParallelsClient(cfg, runner).WaitForGuestExec(ctx, "vm", cfg, time.Minute); !errors.Is(err, cause) {
		t.Fatalf("err=%v", err)
	}
}

type parallelsExecReadyRunner func(context.Context, LocalCommandRequest) (LocalCommandResult, error)

func (f parallelsExecReadyRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	return f(ctx, req)
}

func TestParallelsGuestExecSharesIPStartupBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := Config{TargetOS: targetMacOS, Parallels: ParallelsConfig{StartupTimeout: 7 * time.Second}}
		start := time.Now()
		runner := parallelsExecReadyRunner(func(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
			if req.Args[0] == "list" {
				time.Sleep(5 * time.Second)
				return LocalCommandResult{Stdout: `[{"ID":"vm","State":"running","ip_configured":"10.211.55.8"}]`}, nil
			}
			<-ctx.Done()
			return LocalCommandResult{}, ctx.Err()
		})
		ctx, cancel := ParallelsStartupContext(context.Background(), cfg)
		defer cancel()
		client := NewParallelsClient(cfg, runner)
		vm, err := client.WaitForIP(ctx, "vm", cfg.Parallels.StartupTimeout, ParallelsIPWaitAcquisition)
		if err != nil || vm.IPSource != "tools" {
			t.Fatalf("vm=%+v err=%v", vm, err)
		}
		err = client.WaitForGuestExec(ctx, "vm", cfg, cfg.Parallels.StartupTimeout)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 7*time.Second {
			t.Fatalf("err=%v elapsed=%s", err, time.Since(start))
		}
	})
}
