//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestJoinedLocalCommandSkipsAbsentGroupSignal(t *testing.T) {
	stops := 0
	err := joinLocalCommandProcessGroup(42, func(int) error { stops++; return nil }, func(int) bool { return false }, time.Now(), nil, true)
	if err != nil || stops != 0 {
		t.Fatalf("absent group: signals=%d error=%v", stops, err)
	}
}

func TestJoinedLocalCommandObservesExitBeforeReaping(t *testing.T) {
	t.Setenv(controllerProcessTreeOwnedEnv, "")
	for _, mode := range []string{"fast", "stopped"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], joinedObserverHelperArgs(mode, dir)...)
			release, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer release.Close()
			owner, err := configureJoinedLocalCommand(ctx, cmd, time.Second, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			var observer <-chan error
			observed, waited, allowed := false, false, false
			defer func() {
				if waited {
					return
				}
				_ = cmd.Process.Kill()
				if observer != nil && !observed {
					select {
					case <-observer:
					case <-time.After(5 * time.Second):
						t.Error("exit observer did not join during cleanup")
						return
					}
				}
				_ = owner.join(false)
				if !allowed {
					close(owner.waitReady)
				}
				_ = cmd.Wait()
			}()
			awaitCaptureMarker(t, filepath.Join(dir, "ready"))
			if mode == "stopped" {
				deadline := time.Now().Add(5 * time.Second)
				for {
					state, err := systemInspectionCommand("ps", "-o", "stat=", "-p", strconv.Itoa(cmd.Process.Pid)).Output()
					if err != nil {
						t.Fatal(err)
					}
					if strings.HasPrefix(strings.TrimSpace(string(state)), "T") {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("owned helper did not reach its stopped state")
					}
					time.Sleep(time.Millisecond)
				}
			}
			if mode == "fast" {
				// An initial observation proves the fast exit occurred before
				// the owner starts its own observation, without consuming it.
				for {
					exited, err := observeLocalCommandLeader(cmd.Process.Pid, os.Getpid())
					if err != nil {
						t.Fatal(err)
					}
					if exited {
						break
					}
					time.Sleep(time.Millisecond)
				}
			}
			done := make(chan error, 1)
			observer = done
			go func() { done <- owner.beforeWait() }()
			if mode == "stopped" {
				select {
				case err := <-done:
					observed = true
					t.Fatalf("stopped child was treated as exited: %v", err)
				case <-time.After(30 * time.Millisecond):
				}
				if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
					t.Fatal(err)
				}
				if _, err := release.Write([]byte{1}); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				observed = true
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("exit observation did not complete")
			}
			exited, err := observeLocalCommandLeader(cmd.Process.Pid, os.Getpid())
			if err != nil || !exited {
				t.Fatalf("group cleanup consumed the leader: exited=%t error=%v", exited, err)
			}
			close(owner.waitReady)
			allowed = true
			waitErr := cmd.Wait()
			waited = true
			var processErr *exec.ExitError
			if !errors.As(waitErr, &processErr) || processErr.ExitCode() != 7 {
				t.Fatalf("collecting wait lost the original exit: %v", waitErr)
			}
			// This callback is now late and must reuse the settled Once.
			if err := owner.cancel(); err != nil {
				t.Fatalf("late cancellation changed settled ownership: %v", err)
			}
		})
	}
}

func TestJoinedLocalCommandContradictionRetainsClaimWithoutReap(t *testing.T) {
	dir := t.TempDir()
	claimPath := filepath.Join(dir, "claim.json")
	cmd := exec.Command(os.Args[0], joinedObserverHelperArgs("contradiction", dir)...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The deliberately blocked fixture has no surviving children. Its test
	// owner explicitly kills and collects it; production does not do this.
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	awaitCaptureMarker(t, filepath.Join(dir, "pending"))
	data, err := os.ReadFile(filepath.Join(dir, "pending"))
	if err != nil || !strings.Contains(string(data), "without signal or reap") {
		t.Fatalf("contradiction was not visible: %q error=%v", data, err)
	}
	// Outlast the ordinary 250ms pipe fallback: the existing cancellation
	// watcher must remain behind the unopened reap-permission gate.
	time.Sleep(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := withLeaseClaimLockContext(ctx, claimPath, false, func() error {
		t.Error("contradicted owner released its original claim")
		return nil
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("claim exclusion error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "unsafe-return")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("contradiction proceeded to a later wait: %v", err)
	}
}

func joinedObserverHelperArgs(mode, dir string) []string {
	return []string{"-test.run=^TestJoinedLocalCommandObserverHelper$", "--", mode, dir}
}

func TestJoinedLocalCommandObserverHelper(t *testing.T) {
	index := slices.Index(os.Args, "--")
	if index < 0 {
		return
	}
	mode, dir := os.Args[index+1], os.Args[index+2]
	if mode == "fast" || mode == "stopped" {
		if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("ready"), 0o600); err != nil {
			os.Exit(90)
		}
		if mode == "stopped" {
			_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
			// Signal delivery may lag this thread; only the parent can allow exit.
			var release [1]byte
			if _, err := io.ReadFull(os.Stdin, release[:]); err != nil {
				os.Exit(96)
			}
		}
		os.Exit(7)
	}
	if mode != "contradiction" {
		os.Exit(91)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, os.Args[0], joinedObserverHelperArgs("fast", dir)...)
	cmd.WaitDelay = controllerChildWaitDelay
	owner, err := configureJoinedLocalCommand(ctx, cmd, time.Second, func(err error) {
		pending := filepath.Join(dir, "pending")
		if err := os.WriteFile(pending+".tmp", []byte(err.Error()), 0o600); err != nil {
			os.Exit(97)
		}
		if err := os.Rename(pending+".tmp", pending); err != nil {
			os.Exit(98)
		}
		cancel()
	})
	if err != nil {
		os.Exit(95)
	}
	if err := cmd.Start(); err != nil {
		os.Exit(92)
	}
	// Deliberately violate sole-reaper ownership without updating Go's
	// Process state, as another raw waiter would in an embedding process.
	for {
		var state syscall.WaitStatus
		pid, err := syscall.Wait4(cmd.Process.Pid, &state, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil || pid != cmd.Process.Pid {
			os.Exit(93)
		}
		break
	}
	_ = withLeaseClaimLockContext(context.Background(), filepath.Join(dir, "claim.json"), true, func() error {
		_ = owner.beforeWait()
		_ = os.WriteFile(filepath.Join(dir, "unsafe-return"), []byte("unsafe"), 0o600)
		close(owner.waitReady)
		return cmd.Wait()
	})
	os.Exit(94)
}
