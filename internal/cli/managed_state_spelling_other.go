//go:build !darwin

package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func managedStateEntrySpelling(resolvedParent, name string, info os.FileInfo) (string, error) {
	// Stat alone does not reveal the real spelling on case-insensitive
	// filesystems. Bound this directory-metadata lookup; never read data.
	dir, err := os.Open(resolvedParent)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	found := ""
	for count := 0; count < 65536; count += 256 {
		entries, readErr := dir.ReadDir(256)
		for _, entry := range entries {
			if !strings.EqualFold(entry.Name(), name) {
				continue
			}
			entryInfo, err := entry.Info()
			if err != nil {
				return "", err
			}
			if !os.SameFile(info, entryInfo) {
				continue
			}
			if entry.Name() == name {
				return filepath.Join(resolvedParent, name), nil
			}
			if found != "" {
				return "", fmt.Errorf("ambiguous managed-state transfer path spelling")
			}
			found = entry.Name()
		}
		if readErr == io.EOF {
			if found == "" {
				return "", fmt.Errorf("managed-state transfer path changed during metadata inspection")
			}
			return filepath.Join(resolvedParent, found), nil
		}
		if readErr != nil {
			return "", readErr
		}
	}
	return "", fmt.Errorf("managed-state transfer directory exceeds metadata inspection limit")
}
