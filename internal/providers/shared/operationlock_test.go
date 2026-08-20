package shared

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestLockLeaseOperationPreservesProviderPaths(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	tests := []struct {
		providerName string
		leaseID      string
		lockName     string
	}{
		{providerName: "aws-lambda-microvm", leaseID: "cbx_locktest", lockName: "cbx_locktest.aws-lambda-microvm-operation.lock"},
		{providerName: "cloudflare-sandbox", leaseID: "cfsbx_sandbox", lockName: "cfsbx_sandbox.cloudflare-sandbox-operation.lock"},
		{providerName: "vercel-sandbox", leaseID: "vsbx_sandbox", lockName: "vsbx_sandbox.vercel-sandbox-operation.lock"},
		{providerName: "unikraft-cloud", leaseID: "ukc_aaaaaaaaaaaa", lockName: "ukc_aaaaaaaaaaaa.unikraft-cloud-operation.lock"},
		{providerName: "agent-sandbox", leaseID: "asbx_sandbox", lockName: "asbx_sandbox.agent-sandbox-operation.lock"},
		{providerName: "opensandbox", leaseID: "osbx_sandbox", lockName: "osbx_sandbox.opensandbox-operation.lock"},
		{providerName: "crownest", leaseID: "cnsbx_sandbox", lockName: "cnsbx_sandbox.crownest-operation.lock"},
		{providerName: "superserve", leaseID: "ssbx_sandbox", lockName: "ssbx_sandbox.superserve-operation.lock"},
		{providerName: "github-codespaces", leaseID: "cbx_123456789abc", lockName: "cbx_123456789abc.github-codespaces-operation.lock"},
	}

	for _, test := range tests {
		t.Run(test.providerName, func(t *testing.T) {
			unlock, err := LockLeaseOperation(context.Background(), test.providerName, test.leaseID)
			if err != nil {
				t.Fatalf("lock lease operation: %v", err)
			}
			unlock()

			lockPath := filepath.Join(stateHome, "crabbox", "claim-locks", test.lockName)
			info, err := os.Stat(lockPath)
			if err != nil {
				t.Fatalf("stat exact lock path %q: %v", lockPath, err)
			}
			if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
				t.Fatalf("lock mode = %#o, want %#o", got, 0o600)
			}
		})
	}

	lockDir := filepath.Join(stateHome, "crabbox", "claim-locks")
	info, err := os.Stat(lockDir)
	if err != nil {
		t.Fatalf("stat lock directory: %v", err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o700 {
		t.Fatalf("lock directory mode = %#o, want %#o", got, 0o700)
	}
}

func TestLockLeaseOperationHonorsContextAndUnlockIsIdempotent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const (
		providerName = "test-provider"
		leaseID      = "test_lease"
	)

	unlock, err := LockLeaseOperation(context.Background(), providerName, leaseID)
	if err != nil {
		t.Fatalf("lock first lease operation: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := LockLeaseOperation(ctx, providerName, leaseID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock error = %v, want context deadline exceeded", err)
	}

	unlock()
	unlock()

	second, err := LockLeaseOperation(context.Background(), providerName, leaseID)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	second()
}
