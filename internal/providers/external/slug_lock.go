package external

import (
	"fmt"
	"os"
	"path/filepath"
)

func slugReservationLockPath(path string) (string, error) {
	dir := filepath.Join(filepath.Dir(path), ".locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create external slug reservation lock dir: %w", err)
	}
	return filepath.Join(dir, "reservations.lock"), nil
}
