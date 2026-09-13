package atomicfile

import (
	"os"
	"path/filepath"
)

// WritePrivate stages a complete 0600 file beside path, syncs and closes it,
// then publishes it with replace. A successful replace must consume the staged
// path. Callers retain path admission, parent creation, and directory-sync policy.
func WritePrivate(path, pattern string, data []byte, replace func(string, string) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), pattern)
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
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
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
