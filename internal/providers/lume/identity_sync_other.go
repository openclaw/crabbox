//go:build !windows

package lume

import (
	"os"

	core "github.com/openclaw/crabbox/internal/cli"
)

func syncLumeStorageDirectory(root string) error {
	dir, err := os.Open(root)
	if err != nil {
		return core.Exit(5, "open Lume storage %q for identity sync: %v", root, err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return core.Exit(5, "sync Lume storage identity directory %q: %v", root, syncErr)
	}
	if closeErr != nil {
		return core.Exit(5, "close Lume storage identity directory %q: %v", root, closeErr)
	}
	return nil
}
