package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func waitClaimWriter(t *testing.T, path string) {
	t.Helper()
	lockPath, err := leaseClaimLockPath(path)
	if err != nil {
		t.Fatal(err)
	}
	mu := claimMutationMutex(lockPath)
	deadline := time.After(5 * time.Second)
	for mu.TryLock() {
		mu.Unlock()
		select {
		case <-deadline:
			t.Fatal("writer never acquired its process mutex")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestClaimSharedFenceAllowsCancellationAheadOfWriter(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path, err := leaseClaimPath("cbx_shared_reader")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	written := make(chan error, 1)
	err = withLeaseClaimLockContext(ctx, path, true, func() error {
		go func() { written <- withLeaseClaimLock(path, func() error { return nil }) }()
		waitClaimWriter(t, path)
		return withLeaseClaimLockContext(ctx, path, true, func() error {
			select {
			case err := <-written:
				t.Fatalf("writer crossed active readers: %v", err)
			default:
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("writer did not proceed after readers left")
	}
}

func TestClaimFenceContextBoundsBothWriterWaits(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path, err := leaseClaimPath("cbx_shared_deadline")
	if err != nil {
		t.Fatal(err)
	}
	writerCtx, cancelWriter := context.WithCancel(t.Context())
	defer cancelWriter()
	written := make(chan error, 1)
	err = withLeaseClaimLockContext(t.Context(), path, true, func() error {
		go func() {
			written <- withLeaseClaimLockContext(writerCtx, path, false, func() error { t.Error("writer crossed reader"); return nil })
		}()
		waitClaimWriter(t, path)
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		err := withLeaseClaimLockContext(ctx, path, false, func() error { t.Error("second writer crossed reader"); return nil })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("process mutex wait did not honor deadline: %v", err)
		}
		cancelWriter()
		if err := <-written; !errors.Is(err, context.Canceled) {
			t.Fatalf("file lock wait did not cancel: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := withLeaseClaimLock(path, func() error { return nil }); err != nil {
		t.Fatalf("cancelled acquisition leaked lock: %v", err)
	}
}

func TestClaimTouchCancellationLeavesHeldClaimUnchanged(t *testing.T) {
	for _, action := range []bool{false, true} {
		for _, shared := range []bool{false, true} {
			t.Run(fmt.Sprintf("action=%t/shared=%t", action, shared), func(t *testing.T) {
				expected := seedClaimContract(t)
				path, err := leaseClaimPath(expected.LeaseID)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				called, joined := false, false
				err = withLeaseClaimLockContext(t.Context(), path, shared, func() error {
					ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
					defer cancel()
					go func() {
						var err error
						if action {
							_, _, _, err = UpdateLeaseClaimTouchIfUnchangedAction(ctx, expected.LeaseID, expected, time.Now(), nil, func() (Server, SSHTarget, bool, error) {
								called = true
								return Server{Provider: "aws"}, SSHTarget{}, true, nil
							})
						} else {
							_, err = UpdateLeaseClaimTouchIfUnchanged(ctx, expected.LeaseID, expected, map[string]string{"state": "touched"}, time.Now(), nil)
						}
						done <- err
					}()
					select {
					case err := <-done:
						joined = true
						if !errors.Is(err, context.DeadlineExceeded) {
							t.Errorf("touch deadline returned %v", err)
						}
					case <-time.After(time.Second):
						t.Error("touch ignored cancellation while waiting for the claim")
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if !joined {
					<-done
				}
				if called {
					t.Fatal("canceled touch reached the provider action")
				}
				assertClaimContractStored(t, expected.LeaseID, expected)
			})
		}
	}
}

func TestClaimSharedFenceChild(t *testing.T) {
	path := os.Getenv("CRABBOX_TEST_CLAIM_SHARED_PATH")
	if path == "" {
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if os.Getenv("CRABBOX_TEST_CLAIM_SHARED_MODE") == "writer" {
		done := make(chan error, 1)
		go func() {
			done <- withLeaseClaimLockContext(ctx, path, false, func() error { fmt.Println("WRITTEN"); return nil })
		}()
		waitClaimWriter(t, path)
		fmt.Println("WAITING")
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := withLeaseClaimLockContext(ctx, path, true, func() error {
		fmt.Println("SHARED")
		_, err := bufio.NewReader(os.Stdin).ReadString('\n')
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestClaimSharedFenceAcrossProcesses(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path, err := leaseClaimPath("cbx_shared_processes")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	start := func(mode string) (*exec.Cmd, *bufio.Reader, func()) {
		t.Helper()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestClaimSharedFenceChild$")
		cmd.Env = append(os.Environ(), "CRABBOX_TEST_CLAIM_SHARED_PATH="+path, "CRABBOX_TEST_CLAIM_SHARED_MODE="+mode)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		in, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		})
		return cmd, bufio.NewReader(out), func() { _, _ = fmt.Fprintln(in, "release"); _ = in.Close() }
	}
	read := func(reader *bufio.Reader, want string) {
		t.Helper()
		if got, err := reader.ReadString('\n'); err != nil || got != want+"\n" {
			t.Fatalf("child marker=%q want=%q err=%v", got, want, err)
		}
	}
	first, firstOut, releaseFirst := start("reader")
	read(firstOut, "SHARED")
	writer, writerOut, _ := start("writer")
	read(writerOut, "WAITING")
	second, secondOut, releaseSecond := start("reader")
	read(secondOut, "SHARED")
	releaseFirst()
	if err := first.Wait(); err != nil {
		t.Fatal(err)
	}
	// Releasing one independent shared handle must not release the other.
	deadline, cancelDeadline := context.WithTimeout(ctx, 50*time.Millisecond)
	err = withLeaseClaimLockContext(deadline, path, false, func() error { t.Error("writer crossed second process reader"); return nil })
	cancelDeadline()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exclusive fence bypassed remaining reader: %v", err)
	}
	releaseSecond()
	if err := second.Wait(); err != nil {
		t.Fatal(err)
	}
	read(writerOut, "WRITTEN")
	if err := writer.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestClaimSharedFinalizationPreservesReplacement(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const id = "cbx_shared_replaced"
	if err := claimLeaseForRepoProvider(id, "shared", "blacksmith-testbox", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	before, err := readLeaseClaim(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := withLeaseClaimUnchangedContext(t.Context(), id, before, true, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	replacement := before
	replacement.RepoRoot = filepath.Join(t.TempDir(), "new-owner")
	if err := replaceLeaseClaimIfUnchanged(id, before, replacement); err != nil {
		t.Fatal(err)
	}
	err = cleanupLeaseClaimIfUnchangedAfterContext(t.Context(), id, before, true, func() error { t.Error("replacement authorized cleanup"); return nil }, syncControllerDirectory)
	after, readErr := readLeaseClaim(id)
	if err == nil || readErr != nil || after.RepoRoot != replacement.RepoRoot {
		t.Fatalf("replacement lost: err=%v read=%v after=%+v", err, readErr, after)
	}
}

func TestClaimFenceContextCancelsPublicationWithoutMutation(t *testing.T) {
	for _, operation := range []string{"reuse", "publish", "finalize", "shared"} {
		t.Run(operation, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const id = "cbx_shared_publication"
			repo := t.TempDir()
			if err := claimLeaseForRepoProvider(id, "shared", "blacksmith-testbox", repo, time.Minute, false); err != nil {
				t.Fatal(err)
			}
			claim, err := readLeaseClaim(id)
			if err != nil {
				t.Fatal(err)
			}
			path, _ := leaseClaimPath(id)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			err = withLeaseClaimLock(path, func() error {
				action := func() error { t.Error("cancelled operation entered callback"); return nil }
				switch operation {
				case "reuse":
					cfg := baseConfig()
					cfg.Provider = "blacksmith-testbox"
					_, err := ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfterContext(ctx, id, claim.Slug, cfg, "", Server{}, SSHTarget{}, repo, time.Minute, false, claim, true, action)
					return err
				case "publish":
					return WithDurableLeaseClaimLockContext(ctx, id, func(*LeaseClaim, bool, func() error) error { return action() })
				case "finalize":
					return CleanupLeaseClaimIfUnchangedAfterContext(ctx, id, claim, true, action)
				default:
					return WithLeaseClaimUnchangedShared(ctx, id, claim, action)
				}
			})
			after, readErr := os.ReadFile(path)
			if !errors.Is(err, context.DeadlineExceeded) || readErr != nil || string(before) != string(after) {
				t.Fatalf("cancelled operation changed state: err=%v read=%v", err, readErr)
			}
		})
	}
}
