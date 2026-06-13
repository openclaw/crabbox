//go:build !windows

package external

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestConfirmedAbsentSlugRemovalRequiresDirectorySyncAndRetriesAfterDeletion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reservation.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	syncErr := errors.New("reservation directory sync unavailable")
	err := removeSlugReservationFileWithSync(path, func(string) error { return syncErr })
	if !errors.Is(err, syncErr) {
		t.Fatalf("reservation removal error=%v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("reservation remains after removal sync failure: %v", statErr)
	}
	var synced string
	if err := removeSlugReservationFileWithSync(path, func(got string) error {
		synced = filepath.Clean(got)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if synced != filepath.Clean(dir) {
		t.Fatalf("retry synced %q want %q", synced, dir)
	}
}
