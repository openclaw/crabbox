//go:build windows

package lume

import (
	"fmt"
	"path/filepath"
	"strings"
)

func validateLumeSharedDirectoryPath(path string) error {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return fmt.Errorf("must be an absolute path")
	}
	volume := filepath.VolumeName(clean)
	remainder := strings.TrimPrefix(clean, volume)
	if strings.Contains(remainder, ":") {
		return fmt.Errorf("cannot contain a colon outside the volume name")
	}
	if strings.Contains(volume, ":") && (len(volume) != 2 || volume[1] != ':') {
		return fmt.Errorf("has an unsupported volume name")
	}
	return nil
}
