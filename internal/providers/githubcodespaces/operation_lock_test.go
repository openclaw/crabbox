package githubcodespaces

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitHubCodespacesLeaseOperationLockHonorsContextCancellation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_123456789abc"
	unlocks, err := lockGitHubCodespacesLeaseOperation(context.Background(), leaseID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlocks()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := lockGitHubCodespacesLeaseOperation(ctx, leaseID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context deadline", err)
	}

	unlocks()
	second, err := lockGitHubCodespacesLeaseOperation(context.Background(), leaseID)
	if err != nil {
		t.Fatalf("acquire after unlock: %v", err)
	}
	second()
}

func TestGitHubCodespacesSlugAllocationLockHonorsContextCancellation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	unlocks, err := lockGitHubCodespacesSlugAllocation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlocks()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := lockGitHubCodespacesSlugAllocation(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context deadline", err)
	}
}

func TestGitHubCodespacesLeaseOperationLockAllowsDifferentLeases(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	first, err := lockGitHubCodespacesLeaseOperation(context.Background(), "cbx_123456789abc")
	if err != nil {
		t.Fatal(err)
	}
	defer first()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	second, err := lockGitHubCodespacesLeaseOperation(ctx, "cbx_123456789abd")
	if err != nil {
		t.Fatalf("different lease was blocked: %v", err)
	}
	second()
}

func TestGitHubCodespacesLeaseOperationLockRejectsUnsafeIDs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, leaseID := range []string{
		"",
		"cbx_",
		"cbx_123456789ab",
		"cbx_123456789abcd",
		"cbx_123456789abg",
		"cbx_123456789ABC",
		"cbx_123456789ab/",
		`cbx_123456789ab\`,
		"../cbx_123456789abc",
		"CON",
	} {
		t.Run(strings.ReplaceAll(leaseID, "/", "slash"), func(t *testing.T) {
			unlock, err := lockGitHubCodespacesLeaseOperation(context.Background(), leaseID)
			if err == nil {
				unlock()
				t.Fatalf("unsafe lease id %q acquired a lock", leaseID)
			}
		})
	}
}

func TestGitHubCodespacesOperationLockRejectsRelativeStateDirectory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative-state")
	if unlock, err := lockGitHubCodespacesSlugAllocation(context.Background()); err == nil {
		unlock()
		t.Fatal("relative state directory acquired a lock")
	} else if !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("err=%v", err)
	}
}

func TestGitHubCodespacesOperationLockChecksCanceledContextBeforeFilesystem(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative-state")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lockGitHubCodespacesSlugAllocation(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context canceled", err)
	}
}

func TestGitHubCodespacesOperationLockRequiresClaimNamespace(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	original := ensureGitHubCodespacesClaimNamespace
	defer func() { ensureGitHubCodespacesClaimNamespace = original }()
	ensureGitHubCodespacesClaimNamespace = func(string) error {
		return errors.New("simulated namespace failure")
	}

	if unlock, err := lockGitHubCodespacesSlugAllocation(context.Background()); err == nil {
		unlock()
		t.Fatal("lock acquired without claim namespace")
	} else if !strings.Contains(err.Error(), "simulated namespace failure") {
		t.Fatalf("err=%v", err)
	}
	lockPath := filepath.Join(stateHome, "crabbox", "claim-locks", "github-codespaces-slug-allocation.lock")
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock file exists after namespace failure: %v", err)
	}
}

func TestGitHubCodespacesOperationLocksSerializeAcrossProcesses(t *testing.T) {
	for _, kind := range []string{"lease", "slug"} {
		t.Run(kind, func(t *testing.T) {
			stateHome := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestGitHubCodespacesOperationLockHelper$")
			cmd.Env = append(os.Environ(),
				"XDG_STATE_HOME="+stateHome,
				"CRABBOX_GITHUB_CODESPACES_LOCK_HELPER="+kind,
			)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				if waited {
					return
				}
				_ = stdin.Close()
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			})
			reader := bufio.NewReader(stdout)
			line, err := reader.ReadString('\n')
			if err != nil || strings.TrimSpace(line) != "locked" {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatalf("helper did not acquire lock: line=%q err=%v stderr=%q", line, err, stderr.String())
			}

			t.Setenv("XDG_STATE_HOME", stateHome)
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			_, lockErr := lockGitHubCodespacesTestOperation(ctx, kind)
			cancel()
			if !errors.Is(lockErr, context.DeadlineExceeded) {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatalf("cross-process lock err=%v, want context deadline", lockErr)
			}

			if _, err := fmt.Fprintln(stdin, "release"); err != nil {
				t.Fatal(err)
			}
			if err := stdin.Close(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatalf("helper exit: %v stderr=%q", err, stderr.String())
			}
			waited = true

			unlock, err := lockGitHubCodespacesTestOperation(context.Background(), kind)
			if err != nil {
				t.Fatalf("acquire after helper exit: %v", err)
			}
			unlock()
		})
	}
}

func TestGitHubCodespacesOperationLockHelper(t *testing.T) {
	kind := os.Getenv("CRABBOX_GITHUB_CODESPACES_LOCK_HELPER")
	if kind == "" {
		return
	}
	unlocks, err := lockGitHubCodespacesTestOperation(context.Background(), kind)
	if err != nil {
		t.Fatal(err)
	}
	defer unlocks()
	if _, err := fmt.Fprintln(os.Stdout, "locked"); err != nil {
		t.Fatal(err)
	}
	if !bufio.NewScanner(os.Stdin).Scan() {
		t.Fatal("parent closed stdin before releasing lock")
	}
}

func lockGitHubCodespacesTestOperation(ctx context.Context, kind string) (func(), error) {
	if kind == "slug" {
		return lockGitHubCodespacesSlugAllocation(ctx)
	}
	return lockGitHubCodespacesLeaseOperation(ctx, "cbx_123456789abc")
}
