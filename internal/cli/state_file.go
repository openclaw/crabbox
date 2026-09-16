package cli

import (
	"fmt"
	"path/filepath"

	"github.com/openclaw/crabbox/internal/atomicfile"
)

// Publish the complete record atomically and sync its namespace before remote effects.
func writeStateFileAtomic(path string, data []byte, syncDirectory func(string) error) error {
	dir := filepath.Dir(path)
	if err := atomicfile.WritePrivate(path, "."+filepath.Base(path)+".tmp-*", data, replaceClaimFile); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync state directory %s: %w", dir, err)
	}
	return nil
}
