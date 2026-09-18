//go:build windows

package external

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/openclaw/crabbox/internal/testutil"
)

// An abandoned lock file must be reclaimed and re-acquired, not reported as
// contended. Reporting "not acquired" here also leaks the reopened handle,
// which then blocks its own removal and strands every later reservation.
func TestLockSlugReservationAcquiresReclaimedAbandonedLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reservation.json")
	lockPath, err := slugReservationLockPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte("abandoned"), 0o600); err != nil {
		t.Fatal(err)
	}

	unlock, locked, err := lockSlugReservation(path)
	if err != nil {
		t.Fatalf("lockSlugReservation: %v", err)
	}
	if !locked {
		t.Fatal("abandoned external slug reservation lock was not reclaimed")
	}
	if unlock == nil {
		t.Fatal("reclaimed external slug reservation lock returned no release")
	}

	owner, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(owner) == "abandoned" {
		t.Fatal("reclaimed lock still records the abandoned owner token")
	}

	unlock()
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("released external slug reservation lock remains: %v", err)
	}
}

// The reclaim path must leave the lock reusable rather than stranded behind a
// leaked handle, so a later waiter acquires it instead of timing out.
func TestWaitForSlugReservationLockRecoversAfterAbandonedLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reservation.json")
	lockPath, err := slugReservationLockPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte("abandoned"), 0o600); err != nil {
		t.Fatal(err)
	}

	unlock, err := waitForSlugReservationLock(path, 2*time.Second)
	if err != nil {
		t.Fatalf("waitForSlugReservationLock: %v", err)
	}
	unlock()

	again, err := waitForSlugReservationLock(path, 2*time.Second)
	if err != nil {
		t.Fatalf("waitForSlugReservationLock after release: %v", err)
	}
	again()
}

func TestAllocateLeaseSlugWindowsReleaseAndReuse(t *testing.T) {
	testutil.IsolateUserDirs(t)
	backend := &leaseBackend{cfg: testConfig()}
	dir, err := backend.slugReservationDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureSlugReservationDir(dir); err != nil {
		t.Fatal(err)
	}
	path := slugReservationPath(dir, "shared")
	lockPath, err := slugReservationLockPath(path)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^TestWindowsSlugLockOwnerSubprocess$", "-test.timeout=10s", "--", "abandon-slug-lock", path)
	child.WaitDelay = 5 * time.Second
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("reservation lock owner subprocess: %v\n%s", err, output)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("exited child did not leave its reservation lock: %v", err)
	}
	t.Log("child owner exited normally; its production reservation lock remains for backend recovery")

	for _, phase := range []struct{ name, leaseID string }{
		{"recovery", "cbx_first"},
		{"reuse", "cbx_second"},
	} {
		leaseID := phase.leaseID
		slug, reservation, err := backend.allocateLeaseSlug(leaseID, "shared")
		if err != nil {
			t.Fatalf("allocateLeaseSlug(%s): %v", leaseID, err)
		}
		if reservation == nil {
			t.Fatalf("allocateLeaseSlug(%s) returned no reservation", leaseID)
		}
		t.Cleanup(reservation.Release)
		if slug != "shared" {
			t.Fatalf("allocated slug=%q, want shared", slug)
		}
		data, err := os.ReadFile(reservation.path)
		if err != nil {
			t.Fatal(err)
		}
		var record slugReservationRecord
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		if record.LeaseID != leaseID || record.Slug != slug {
			t.Fatalf("persisted reservation does not match lease %s and slug %s", leaseID, slug)
		}
		if reservation.path != path {
			t.Fatal("backend allocated a different reservation path")
		}
		if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
			t.Fatalf("allocation left the reservation lock behind: %v", err)
		}

		reservation.Release()
		for _, path := range []string{reservation.path, lockPath} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("release left reservation state at %s: %v", path, err)
			}
		}
		t.Logf("backend %s succeeded; reservation and lock released", phase.name)
	}
}

func TestWindowsSlugLockOwnerSubprocess(t *testing.T) {
	args := flag.Args()
	if len(args) != 2 || args[0] != "abandon-slug-lock" {
		return
	}
	unlock, locked, err := lockSlugReservation(args[1])
	if err != nil {
		t.Fatal(err)
	}
	if !locked || unlock == nil {
		t.Fatal("child did not acquire its reservation lock")
	}
	// Retain the real owner's handle through this test, then let normal process
	// exit close it without running the reservation lock's release closure.
	defer runtime.KeepAlive(unlock)
}
