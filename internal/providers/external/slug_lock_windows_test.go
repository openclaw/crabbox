//go:build windows

package external

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	for _, leaseID := range []string{"cbx_first", "cbx_second"} {
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
		lockPath, err := slugReservationLockPath(reservation.path)
		if err != nil {
			t.Fatal(err)
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
	}
}
