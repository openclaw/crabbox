//go:build windows

package external

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveSlugReservationFileRecoversDeterministicTombstone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reservation.json")
	tombstone := path + ".deleted"
	if err := os.WriteFile(tombstone, []byte("stale reservation"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeSlugReservationFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tombstone); !os.IsNotExist(err) {
		t.Fatalf("reservation tombstone remains: %v", err)
	}
}

func TestRemoveSlugReservationFileDeletesOriginalAndTombstone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reservation.json")
	if err := os.WriteFile(path, []byte("reservation"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeSlugReservationFile(path); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + ".deleted"} {
		if _, err := os.Stat(candidate); !os.IsNotExist(err) {
			t.Fatalf("removed path %s still exists: %v", candidate, err)
		}
	}
}
