package atomicfile

import (
	"os"
	"path/filepath"

	"github.com/openclaw/crabbox/internal/privatefile"
)

// WritePrivate stages a complete private file beside path, syncs and closes it,
// then publishes it with replace. A successful replace must consume the staged
// path. Callers retain path admission, parent creation, and directory-sync policy.
func WritePrivate(path, pattern string, data []byte, replace func(string, string) error) error {
	tmp, err := privatefile.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return err
	}
	staged := tmp.Name()
	published := false
	defer func() {
		if !published {
			_ = os.Remove(staged)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replace(staged, path); err != nil {
		return err
	}
	published = true
	return nil
}
