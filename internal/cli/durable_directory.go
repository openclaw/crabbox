package cli

import (
	"os"
	"path/filepath"
	"strings"
)

func ensurePrivateDirectoryDurableWithSync(dir, logicalRoot string, syncDirectory func(string) error) error {
	boundary, err := privateDirectoryDurabilityBoundary(dir, logicalRoot)
	if err != nil {
		return err
	}
	return ensurePrivateDirectoryDurableWithinWithSync(dir, boundary, syncDirectory)
}

func privateDirectoryDurabilityBoundary(dir, logicalRoot string) (string, error) {
	if filepath.Separator == '\\' && (isWindowsDevicePath(dir) || isWindowsDevicePath(logicalRoot)) {
		return "", exit(2, "Windows device paths are not supported for private directory durability: dir=%s root=%s", dir, logicalRoot)
	}
	dir = filepath.Clean(dir)
	logicalRoot = filepath.Clean(logicalRoot)
	if !filepath.IsAbs(dir) || !filepath.IsAbs(logicalRoot) {
		return "", exit(2, "private directory and logical root must be absolute: dir=%s root=%s", dir, logicalRoot)
	}
	if !pathWithinRoot(dir, logicalRoot) {
		return "", exit(2, "private directory %s is outside logical root %s", dir, logicalRoot)
	}
	// Prefer the outer temporary root before HOME so a HOME created beneath it
	// cannot narrow the boundary after an interrupted first attempt.
	for _, candidate := range []string{os.TempDir(), userHomeDirectory()} {
		candidate = filepath.Clean(candidate)
		if candidate == "." || filepath.Dir(candidate) == candidate || !pathWithinRoot(logicalRoot, candidate) {
			continue
		}
		info, err := os.Stat(candidate)
		if err != nil || !info.IsDir() {
			return "", exit(2, "trusted private directory boundary %s is unavailable", candidate)
		}
		return candidate, nil
	}
	return stableTopLevelDirectoryBoundary(logicalRoot)
}

func isWindowsDevicePath(path string) bool {
	prefix := path
	if len(prefix) > 4 {
		prefix = prefix[:4]
	}
	prefix = strings.ReplaceAll(prefix, "/", `\`)
	return strings.HasPrefix(prefix, `\\?\`) ||
		strings.HasPrefix(prefix, `\??\`) ||
		strings.HasPrefix(prefix, `\\.\`)
}

func stableTopLevelDirectoryBoundary(path string) (string, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return "", exit(2, "private directory logical root %s must be absolute", path)
	}
	volume := filepath.VolumeName(path)
	root := volume + string(filepath.Separator)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", exit(2, "private directory logical root %s has no safe top-level boundary", path)
	}
	first, _, _ := strings.Cut(relative, string(filepath.Separator))
	boundary := filepath.Join(root, first)
	info, err := os.Stat(boundary)
	if err != nil {
		return "", exit(2, "inspect private directory top-level boundary %s: %v", boundary, err)
	}
	if !info.IsDir() || filepath.Dir(boundary) == boundary {
		return "", exit(2, "private directory top-level boundary %s is not a safe existing directory", boundary)
	}
	return boundary, nil
}

func userHomeDirectory() string {
	home, _ := os.UserHomeDir()
	return home
}

func ensurePrivateDirectoryDurableWithinWithSync(dir, boundary string, syncDirectory func(string) error) error {
	dir = filepath.Clean(dir)
	boundary = filepath.Clean(boundary)
	info, err := os.Stat(boundary)
	if err != nil || !info.IsDir() || filepath.Dir(boundary) == boundary {
		return exit(2, "private directory boundary %s is not a safe existing directory", boundary)
	}
	if !pathWithinRoot(dir, boundary) {
		return exit(2, "private directory %s is outside durability boundary %s", dir, boundary)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return exit(2, "create private directory %s: %v", dir, err)
	}
	if dir == boundary {
		return nil
	}
	for current := filepath.Dir(dir); ; current = filepath.Dir(current) {
		if err := syncDirectory(current); err != nil {
			return exit(2, "sync private directory namespace parent %s: %v", current, err)
		}
		if current == boundary {
			return nil
		}
		if parent := filepath.Dir(current); parent == current {
			return exit(2, "sync private directory namespace: boundary %s is not an ancestor of %s", boundary, dir)
		}
	}
}

func ensureCrabboxClaimNamespaceDurable() error {
	return ensureCrabboxClaimNamespaceDurableWithSync(syncControllerDirectory)
}

func ensureCrabboxClaimNamespaceDurableWithSync(syncDirectory func(string) error) error {
	stateDir, err := crabboxStateDir()
	if err != nil {
		return err
	}
	stateRoot, err := crabboxStateRootDir()
	if err != nil {
		return err
	}
	return ensurePrivateDirectoryDurableWithSync(filepath.Join(stateDir, "claims"), stateRoot, syncDirectory)
}
