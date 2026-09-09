//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestJoinedLocalCommandRejectsControllerBeforeLaunch(t *testing.T) {
	t.Setenv(controllerProcessTreeOwnedEnv, "1")
	if err := ValidateLocalCommandProcessGroupJoin(t.Context()); err == nil {
		t.Fatal("controller-owned command passed the preflight")
	}
	dir := t.TempDir()
	result, err := (execCommandRunner{}).Run(t.Context(), LocalCommandRequest{
		Name: os.Args[0], Args: joinedCommandHelperArgs("writer", dir), RequireProcessGroupJoin: true,
	})
	if err == nil || result.ExitCode == 0 {
		t.Fatalf("controller-owned command result=%+v error=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ready")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported command launched: %v", err)
	}
	if os.Getenv(controllerProcessTreeOwnedEnv) != "1" {
		t.Fatal("preflight changed enclosing controller ownership")
	}
}

func TestJoinedLocalCommandRejectsUnavailableInspectionBeforeLaunch(t *testing.T) {
	for _, mode := range []string{"missing", "unsupported-flags", "malformed-output", "empty-output", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(controllerProcessTreeOwnedEnv, "")
			tools, dir := t.TempDir(), t.TempDir()
			t.Setenv("PATH", tools)
			ctx := t.Context()
			if mode == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			if mode != "missing" {
				script := "#!/bin/sh\nexit 0\n"
				if mode == "unsupported-flags" {
					script = "#!/bin/sh\nprintf 'unsupported flags\\n' >&2\nexit 1\n"
				} else if mode == "malformed-output" {
					script = "#!/bin/sh\nprintf 'PID USER COMMAND\\n'\n"
				} else if mode == "deadline" {
					script = "#!/bin/sh\nexec /bin/sleep 10\n"
				}
				if err := os.WriteFile(filepath.Join(tools, "ps"), []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := ValidateLocalCommandProcessGroupJoin(ctx); err == nil || (mode != "deadline" && !strings.Contains(err.Error(), "ps")) || (mode == "deadline" && !errors.Is(err, context.DeadlineExceeded)) {
				t.Fatalf("unusable ps passed startup validation: %v", err)
			}
			result, err := (execCommandRunner{}).Run(ctx, LocalCommandRequest{
				Name: os.Args[0], Args: joinedCommandHelperArgs("writer", dir), RequireProcessGroupJoin: true,
			})
			if err == nil || result.ExitCode == 0 || (mode != "deadline" && !strings.Contains(err.Error(), "ps")) {
				t.Fatalf("unusable ps reached the joined workload: result=%+v error=%v", result, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "ready")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsupported command launched: %v", err)
			}
		})
	}
}

func TestJoinedLocalCommandAcceptsUnknownProcessState(t *testing.T) {
	t.Setenv(controllerProcessTreeOwnedEnv, "")
	ps, err := exec.LookPath("ps")
	if err != nil {
		t.Fatal(err)
	}
	tools := t.TempDir()
	// Darwin can report a transient unknown state. It remains potentially live.
	script := "#!/bin/sh\n" + shellQuote(ps) + " \"$@\" || exit $?\nprintf '0 ?\\n'\n"
	if err := os.WriteFile(filepath.Join(tools, "ps"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools)
	if err := ValidateLocalCommandProcessGroupJoin(t.Context()); err != nil {
		t.Fatal(err)
	}
	result, err := (execCommandRunner{}).Run(t.Context(), LocalCommandRequest{
		Name: os.Args[0], Args: joinedCommandHelperArgs("normal", t.TempDir()), RequireProcessGroupJoin: true,
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("unknown unrelated process state rejected normal completion: result=%+v error=%v", result, err)
	}
}

func TestJoinedLocalCommandInspectionLossRetainsOriginalClaim(t *testing.T) {
	t.Setenv(controllerProcessTreeOwnedEnv, "")
	ps, err := exec.LookPath("ps")
	if err != nil {
		t.Fatal(err)
	}
	tools, dir := t.TempDir(), t.TempDir()
	loss, claim := filepath.Join(tools, "inspection-lost"), filepath.Join(dir, "claim.json")
	script := "#!/bin/sh\n[ ! -f " + shellQuote(loss) + " ] || exit 1\nexec " + shellQuote(ps) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(tools, "ps"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools)
	ctx, cancel := context.WithCancel(t.Context())
	pending, done := make(chan error, 1), make(chan error, 1)
	returned := false
	defer func() {
		_ = os.Remove(loss)
		cancel()
		if !returned {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("restored observer did not close the original owner")
			}
		}
	}()
	go func() {
		done <- withLeaseClaimLockContext(t.Context(), claim, true, func() error {
			_, err := (execCommandRunner{}).Run(ctx, LocalCommandRequest{
				Name: os.Args[0], Args: joinedCommandHelperArgs("writer", dir), RequireProcessGroupJoin: true,
				CancelGracePeriod: 50 * time.Millisecond, OnCleanupPending: func(err error) { pending <- err },
			})
			return err
		})
	}()
	awaitCaptureMarker(t, filepath.Join(dir, "ready"))
	if err := os.WriteFile(loss, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-pending:
		if !strings.Contains(err.Error(), "cleanup pending") || !strings.Contains(err.Error(), "ps") {
			t.Fatalf("inspection loss not reported: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("inspection loss did not report cleanup pending")
	}
	writerCtx, cancelWriter := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancelWriter()
	if err := withLeaseClaimLockContext(writerCtx, claim, false, func() error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("claim was released without observation: %v", err)
	}
	if err := os.Remove(loss); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		returned = true
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("restored observation lost original cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restored inspection did not join")
	}
	if err := withLeaseClaimLockContext(t.Context(), claim, false, func() error { return nil }); err != nil {
		t.Fatalf("joined owner left the claim locked: %v", err)
	}
}

func TestJoinedLocalCommandPreservesConfiguredPipeWait(t *testing.T) {
	t.Setenv(controllerProcessTreeOwnedEnv, "")
	for _, boundedCapture := range []bool{true, false} {
		cmd := exec.CommandContext(t.Context(), "unused")
		cmd.WaitDelay = time.Second
		want := time.Second
		if boundedCapture {
			configureBoundedCommandCancellation(cmd)
			want = controllerChildWaitDelay
		}
		owner, err := configureJoinedLocalCommand(t.Context(), cmd, time.Second, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cmd.WaitDelay != want || owner.grace != time.Second {
			t.Fatalf("bounded capture=%t: pipe wait=%s group grace=%s", boundedCapture, cmd.WaitDelay, owner.grace)
		}
	}
}

func TestJoinedLocalCommandClosesFiniteDescendants(t *testing.T) {
	t.Setenv(controllerProcessTreeOwnedEnv, "")
	for _, tc := range []struct {
		mode  string
		files bool
	}{
		{mode: "normal"}, {mode: "failure"}, {mode: "parent-pipes"},
		{mode: "parent-hidden"}, {mode: "parent-failure"}, {mode: "cancel"}, {mode: "overflow"},
		{mode: "parent-pipes", files: true}, {mode: "cancel", files: true},
	} {
		t.Run(fmt.Sprintf("%s/files=%t", tc.mode, tc.files), func(t *testing.T) {
			dir := t.TempDir()
			t.Cleanup(func() {
				data, err := os.ReadFile(filepath.Join(dir, "ready"))
				if errors.Is(err, os.ErrNotExist) {
					return
				}
				group, parseErr := strconv.Atoi(string(data))
				if err != nil || parseErr != nil || group <= 0 {
					t.Errorf("read owned fixture group: data=%q read=%v parse=%v", data, err, parseErr)
					return
				}
				// The helper is finite; never signal a post-reap integer from
				// regression cleanup, even if the tested owner returned early.
				if err := waitForControllerProcessGroupExit(group, nil, controllerProcessGroupAlive, time.Now().Add(5*time.Second)); err != nil {
					t.Errorf("finite fixture group did not close: %v", err)
				}
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type outcome struct {
				result LocalCommandResult
				err    error
			}
			done := make(chan outcome, 1)
			returned := false
			defer func() {
				cancel()
				if !returned {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("finite command did not join during test cleanup")
					}
				}
			}()
			go func() {
				result, err := (execCommandRunner{}).Run(ctx, LocalCommandRequest{
					Name: os.Args[0], Args: joinedCommandHelperArgs(tc.mode, dir),
					// Direct-exit helpers must retain the Go driver's shared coverage directory.
					Env:                     []string{"HOME=" + dir, "PATH=/usr/bin:/bin", "GOCOVERDIR=" + os.Getenv("GOCOVERDIR")},
					RequireProcessGroupJoin: true, CancelGracePeriod: 50 * time.Millisecond,
					MaxCapturedOutputBytes: 16, CaptureOutputToFiles: tc.files,
				})
				done <- outcome{result, err}
			}()
			if tc.mode == "cancel" {
				awaitCaptureMarker(t, filepath.Join(dir, "ready"))
				cancel()
			}
			var got outcome
			select {
			case got = <-done:
				returned = true
			case <-time.After(5 * time.Second):
				t.Fatal("finite command did not close")
			}
			switch tc.mode {
			case "normal":
				if got.err != nil || got.result.ExitCode != 0 {
					t.Fatalf("normal completion=%+v error=%v", got.result, got.err)
				}
			case "failure":
				if got.result.ExitCode != 7 || !IsPlainLocalCommandExit(got.result, got.err) {
					t.Fatalf("ordinary failure was changed: %+v error=%v", got.result, got.err)
				}
			case "cancel":
				if !errors.Is(got.err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", got.err)
				}
			case "overflow":
				if got.err == nil || got.result.ExitCode != 5 {
					t.Fatalf("tiny overflow=%+v error=%v", got.result, got.err)
				}
			case "parent-pipes", "parent-hidden", "parent-failure":
				if !errors.Is(got.err, errLocalCommandDescendants) || got.result.ExitCode == 0 {
					t.Fatalf("abandoned descendants lost their lifecycle failure: %+v error=%v", got.result, got.err)
				}
				if tc.mode == "parent-failure" && (got.result.ExitCode != 7 || !strings.HasPrefix(got.err.Error(), "exit status 7")) {
					t.Fatalf("original exit failure was not primary: %+v error=%v", got.result, got.err)
				}
			}
			if tc.mode == "normal" || tc.mode == "failure" {
				return
			}
			data, err := os.ReadFile(filepath.Join(dir, "ready"))
			if err != nil {
				t.Fatal(err)
			}
			group, err := strconv.Atoi(string(data))
			if err != nil || group <= 0 {
				t.Fatalf("invalid owned fixture group %q", data)
			}
			if controllerProcessGroupAlive(group) {
				t.Fatal("Run returned with a live owned group")
			}
			before, err := os.ReadFile(filepath.Join(dir, "writes"))
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(30 * time.Millisecond)
			after, err := os.ReadFile(filepath.Join(dir, "writes"))
			if err != nil || string(after) != string(before) {
				t.Fatalf("destination changed after return: before=%d after=%d err=%v", len(before), len(after), err)
			}
		})
	}
}

func TestJoinedLocalCommandGraceExpiryKeepsOriginalSharedFence(t *testing.T) {
	claimPath := filepath.Join(t.TempDir(), "claim.json")
	var alive atomic.Bool
	alive.Store(true)
	pending := make(chan error, 2)
	done := make(chan error, 1)
	returned := false
	go func() {
		done <- withLeaseClaimLockContext(t.Context(), claimPath, true, func() error {
			return joinLocalCommandProcessGroup(42, func(int) error { return syscall.EPERM }, func(int) bool { return alive.Load() }, time.Now().Add(-time.Millisecond), func(err error) { pending <- err }, false)
		})
	}()
	defer func() {
		alive.Store(false)
		if !returned {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("claim owner did not join during test cleanup")
			}
		}
	}()
	select {
	case err := <-pending:
		if !errors.Is(err, syscall.EPERM) || !strings.Contains(err.Error(), "cleanup pending") {
			t.Fatalf("pending failure=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup grace expiry was not reported")
	}
	writerCtx, cancelWriter := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancelWriter()
	if err := withLeaseClaimLockContext(writerCtx, claimPath, false, func() error {
		t.Error("claim writer crossed the still-live group")
		return nil
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked claim writer error=%v", err)
	}
	select {
	case err := <-done:
		returned = true
		t.Fatalf("join returned before closure: %v", err)
	default:
	}
	alive.Store(false)
	select {
	case err := <-done:
		returned = true
		if err == nil || !errors.Is(err, syscall.EPERM) {
			t.Fatalf("late closure erased the first failure: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("closed group did not release the original claim")
	}
	if len(pending) != 0 {
		t.Fatal("cleanup pending was reported more than once")
	}
	if err := withLeaseClaimLockContext(t.Context(), claimPath, false, func() error { return nil }); err != nil {
		t.Fatalf("joined group left the claim locked: %v", err)
	}
}

func joinedCommandHelperArgs(mode, dir string) []string {
	return []string{"-test.run=^TestJoinedLocalCommandHelper$", "--", mode, dir}
}

func TestJoinedLocalCommandHelper(t *testing.T) {
	index := slices.Index(os.Args, "--")
	if index < 0 {
		return
	}
	mode, dir := os.Args[index+1], os.Args[index+2]
	if mode == "normal" {
		_, _ = os.Stdout.WriteString("ok")
		os.Exit(0)
	}
	if mode == "failure" {
		os.Exit(7)
	}
	if mode == "writer" || mode == "writer-hidden" {
		if mode == "writer-hidden" {
			_ = os.Stdout.Close()
			_ = os.Stderr.Close()
		}
		file, err := os.Create(filepath.Join(dir, "writes"))
		if err != nil {
			os.Exit(90)
		}
		if err := os.WriteFile(filepath.Join(dir, "ready"), []byte(strconv.Itoa(syscall.Getpgrp())), 0o600); err != nil {
			os.Exit(91)
		}
		for range 40 {
			_, _ = file.WriteString("x")
			time.Sleep(25 * time.Millisecond)
		}
		_ = file.Close()
		os.Exit(0)
	}
	childMode := "writer"
	if mode == "parent-hidden" || mode == "parent-failure" {
		childMode = "writer-hidden"
	}
	child := exec.Command(os.Args[0], joinedCommandHelperArgs(childMode, dir)...)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		os.Exit(92)
	}
	awaitCaptureMarker(t, filepath.Join(dir, "ready"))
	if mode == "parent-pipes" || mode == "parent-hidden" {
		os.Exit(0)
	}
	if mode == "parent-failure" {
		os.Exit(7)
	}
	if mode == "overflow" {
		_, _ = os.Stdout.WriteString("seventeen-bytes!!x")
	}
	_ = child.Wait()
	os.Exit(0)
}
