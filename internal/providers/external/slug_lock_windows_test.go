//go:build windows

package external

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
