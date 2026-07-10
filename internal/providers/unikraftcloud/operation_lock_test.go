package unikraftcloud

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	unikraftCloudLockHelperEnv   = "CRABBOX_TEST_UNIKRAFT_CLOUD_LOCK_HELPER"
	unikraftCloudLockHelperLease = "CRABBOX_TEST_UNIKRAFT_CLOUD_LOCK_LEASE"
)

func TestUnikraftCloudSlugAllocationLockSerializesInProcess(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	unlockFirst, err := lockUnikraftCloudSlugAllocation(context.Background())
	if err != nil {
		t.Fatalf("lock first slug allocation: %v", err)
	}

	type result struct {
		unlock func()
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		unlock, lockErr := lockUnikraftCloudSlugAllocation(context.Background())
		resultCh <- result{unlock: unlock, err: lockErr}
	}()

	select {
	case second := <-resultCh:
		if second.unlock != nil {
			second.unlock()
		}
		t.Fatalf("second slug allocation acquired while first lock was held: %v", second.err)
	case <-time.After(100 * time.Millisecond):
	}

	unlockFirst()
	unlockFirst()

	select {
	case second := <-resultCh:
		if second.err != nil {
			t.Fatalf("lock second slug allocation: %v", second.err)
		}
		second.unlock()
	case <-time.After(2 * time.Second):
		t.Fatal("second slug allocation did not acquire after release")
	}
}

func TestUnikraftCloudLeaseOperationLockCancellation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := leasePrefix + testInstanceUUID

	unlock, err := lockUnikraftCloudLeaseOperation(context.Background(), leaseID)
	if err != nil {
		t.Fatalf("lock lease operation: %v", err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := lockUnikraftCloudLeaseOperation(ctx, leaseID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lease lock error = %v, want context deadline exceeded", err)
	}
}

func TestUnikraftCloudLeaseOperationLockRejectsUnsafeIDs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	for _, leaseID := range []string{
		"",
		leasePrefix,
		"other_" + testInstanceUUID,
		leasePrefix + "../escape",
		leasePrefix + `..\escape`,
	} {
		t.Run(leaseID, func(t *testing.T) {
			if unlock, err := lockUnikraftCloudLeaseOperation(context.Background(), leaseID); err == nil {
				unlock()
				t.Fatalf("lock accepted unsafe lease id %q", leaseID)
			}
		})
	}
}

func TestUnikraftCloudLeaseOperationLockSerializesAcrossProcesses(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateRoot)
	leaseID := leasePrefix + testInstanceUUID

	cmd := exec.Command(os.Args[0], "-test.run=^TestUnikraftCloudOperationLockHelper$")
	cmd.Env = append(os.Environ(),
		unikraftCloudLockHelperEnv+"=1",
		unikraftCloudLockHelperLease+"="+leaseID,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("helper stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		_ = stdin.Close()
		_ = cmd.Wait()
		t.Fatalf("helper did not acquire lock: line=%q err=%v stderr=%q", line, err, stderr.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	_, lockErr := lockUnikraftCloudLeaseOperation(ctx, leaseID)
	cancel()
	if !errors.Is(lockErr, context.DeadlineExceeded) {
		t.Fatalf("contended cross-process lock error = %v, want context deadline exceeded", lockErr)
	}

	if err := stdin.Close(); err != nil {
		t.Fatalf("release helper lock: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for helper: %v; stderr=%q", err, stderr.String())
	}

	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	unlock, err := lockUnikraftCloudLeaseOperation(ctx, leaseID)
	if err != nil {
		t.Fatalf("lock after helper release: %v", err)
	}
	unlock()
}

func TestUnikraftCloudOperationLockHelper(t *testing.T) {
	if os.Getenv(unikraftCloudLockHelperEnv) != "1" {
		return
	}

	leaseID := os.Getenv(unikraftCloudLockHelperLease)
	unlock, err := lockUnikraftCloudLeaseOperation(context.Background(), leaseID)
	if err != nil {
		t.Fatalf("lock helper lease operation: %v", err)
	}
	defer unlock()
	if _, err := fmt.Fprintln(os.Stdout, "locked"); err != nil {
		t.Fatalf("signal helper lock: %v", err)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatalf("wait for helper release: %v", err)
	}
}
