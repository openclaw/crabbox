package runnerfs

import (
	"os"
	"path/filepath"
	"strings"
)

func physicalAbsolutePath(name string) (string, error) {
	if !filepath.IsAbs(name) {
		workingDirectory, err := os.Getwd()
		if err != nil {
			return "", err
		}
		if filepath.Separator == '\\' {
			volume := filepath.VolumeName(name)
			if volume != "" {
				// Resolve the drive's current directory, not the rest of the path.
				workingDirectory, err = filepath.Abs(volume + ".")
				if err != nil {
					return "", err
				}
				name = name[len(volume):]
			} else if len(name) > 0 && os.IsPathSeparator(name[0]) {
				name = filepath.VolumeName(workingDirectory) + name
			}
		}
		if !filepath.IsAbs(name) {
			workingDirectory, err = filepath.EvalSymlinks(workingDirectory)
			if err != nil {
				return "", err
			}
			// Join would clean alias/.. before its symlink is resolved.
			name = strings.TrimRight(workingDirectory, string(filepath.Separator)) + string(filepath.Separator) + name
		}
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", err
	}
	// Windows symlinks may resolve to a drive-less rooted path.
	return filepath.Abs(resolved)
}

// Resolve existing parents before cleaning: alias/.. names the physical parent
// of the alias target. Keep the final component untouched for Lstat's link check.
func resolvePathParent(name string) (string, error) {
	volume := filepath.VolumeName(name)
	for len(name) > len(volume)+1 && (name[len(name)-1] == '/' || filepath.Separator == '\\' && name[len(name)-1] == '\\') {
		name = name[:len(name)-1]
	}
	parent, leaf := filepath.Split(name)
	if parent == "" {
		parent = "."
	}
	abs, err := physicalAbsolutePath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(abs, leaf), nil
}
