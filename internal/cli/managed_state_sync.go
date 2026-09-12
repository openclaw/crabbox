package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// NormalizeManagedStateTransferRoot resolves metadata only, including an absent
// suffix. Native owners use it to retain the actual scope of a created mount.
func NormalizeManagedStateTransferRoot(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("managed-state transfer scope must be absolute")
	}
	if filepath.Separator == '\\' && isWindowsDevicePath(root) {
		return "", fmt.Errorf("managed-state transfer scope does not support Windows device paths")
	}
	for _, component := range strings.Split(filepath.ToSlash(root), "/") {
		if component == ".." {
			return "", fmt.Errorf("managed-state transfer scope contains ambiguous parent traversal")
		}
	}
	return normalizeManagedTransferPath(filepath.Clean(root), 0)
}

func normalizeManagedTransferPath(path string, depth int) (string, error) {
	if depth > 128 {
		return "", fmt.Errorf("managed-state transfer path exceeds metadata depth limit")
	}
	info, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		if runtime.GOOS == "windows" {
			path = strings.ToUpper(filepath.VolumeName(path)) + string(filepath.Separator)
		}
		return path, err
	}
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", err
		}
		return normalizeManagedTransferPath(resolved, depth+1)
	}
	if err == nil && runtime.GOOS == "windows" && info.Mode()&os.ModeIrregular != 0 {
		// Go reports Windows junctions as irregular, not symlinks. Readlink
		// recognizes junctions; unsupported reparse types remain an error.
		resolved, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(parent, resolved)
		}
		if _, err := os.Stat(resolved); err != nil {
			return "", err
		}
		return normalizeManagedTransferPath(resolved, depth+1)
	}
	resolvedParent, parentErr := normalizeManagedTransferPath(parent, depth+1)
	if parentErr != nil {
		return "", parentErr
	}
	name := filepath.Base(path)
	if err == nil && runtime.GOOS == "windows" {
		// Directory entries use long names even when the input uses an 8.3
		// alias. Keep the original identity for the comparison below.
		resolved, err := filepath.EvalSymlinks(filepath.Join(resolvedParent, name))
		if err != nil {
			return "", err
		}
		name = filepath.Base(resolved)
	}
	if err == nil && (runtime.GOOS == "darwin" || runtime.GOOS == "windows") {
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
	return filepath.Join(resolvedParent, name), nil
}

func selectedManagedStateNamespace() (string, error) {
	root := os.Getenv("XDG_STATE_HOME")
	if root == "" {
		return "", nil
	}
	return NormalizeManagedStateTransferRoot(filepath.Join(root, "crabbox"))
}

func managedPathContains(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(rel) && filepath.Join(root, rel) == filepath.Clean(path)
}

func managedTransferRoots(source string) (string, string, error) {
	managed, err := selectedManagedStateNamespace()
	if err != nil || managed == "" {
		return "", "", err
	}
	source, err = NormalizeManagedStateTransferRoot(source)
	if err != nil {
		return "", "", err
	}
	if (runtime.GOOS == "darwin" || runtime.GOOS == "windows") &&
		!managedPathContains(source, managed) && !managedPathContains(managed, source) &&
		(managedPathContains(strings.ToLower(source), strings.ToLower(managed)) || managedPathContains(strings.ToLower(managed), strings.ToLower(source))) {
		if _, err := os.Lstat(source); err != nil {
			return "", "", fmt.Errorf("ambiguous casing in managed-state transfer scope")
		}
		if _, err := os.Lstat(managed); err != nil {
			return "", "", fmt.Errorf("ambiguous casing in selected managed namespace")
		}
	}
	return source, managed, nil
}

// ValidateManagedStateTransferScope is for transports that cannot exclude a
// subtree. Empty/unset XDG_STATE_HOME preserves their existing behavior.
func ValidateManagedStateTransferScope(scope string, roots ...string) error {
	if os.Getenv("XDG_STATE_HOME") == "" {
		return nil
	}
	for _, root := range roots {
		source, managed, err := managedTransferRoots(root)
		if err != nil {
			return exit(2, "%s: cannot establish managed-state transfer scope: %v", scope, err)
		}
		if managedPathContains(source, managed) || managedPathContains(managed, source) {
			return exit(2, "%s cannot exclude the selected managed state namespace from this source or mount; choose a state root outside that scope", scope)
		}
	}
	return nil
}

func managedStateSyncSubtree(root string) (string, error) {
	source, managed, err := managedTransferRoots(root)
	if err != nil || managed == "" {
		return "", err
	}
	if managedPathContains(managed, source) {
		return "", exit(6, "sync source is inside the selected managed state namespace")
	}
	if !managedPathContains(source, managed) {
		return "", nil
	}
	rel, err := filepath.Rel(source, managed)
	return filepath.ToSlash(rel), err
}

type managedSyncParent struct {
	path string
	info os.FileInfo
}

type managedSyncScope struct {
	input, source, namespace string
	parents                  map[string]managedSyncParent
}

func newManagedSyncScope(root string) (*managedSyncScope, error) {
	source, namespace, err := managedTransferRoots(root)
	if err != nil {
		return nil, err
	}
	if namespace != "" && managedPathContains(namespace, source) {
		return nil, exit(6, "sync source is inside the selected managed state namespace")
	}
	return &managedSyncScope{input: root, source: source, namespace: namespace, parents: map[string]managedSyncParent{}}, nil
}

func (scope *managedSyncScope) contains(rel string) (bool, error) {
	if scope.namespace == "" {
		return false, nil
	}
	if !safeRepoRel(filepath.ToSlash(rel)) {
		return false, nil
	}
	full := filepath.Join(scope.input, filepath.FromSlash(rel))
	if managedPathContains(scope.namespace, full) {
		return true, nil
	}
	parent := filepath.Dir(full)
	info, statErr := os.Stat(parent)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return false, statErr
	}
	cached, ok := scope.parents[parent]
	if !ok || statErr != nil || !os.SameFile(info, cached.info) || !info.ModTime().Equal(cached.info.ModTime()) {
		resolved, err := NormalizeManagedStateTransferRoot(parent)
		if err != nil {
			return false, err
		}
		cached = managedSyncParent{path: resolved, info: info}
		if statErr == nil {
			scope.parents[parent] = cached
		}
	}
	// Resolve the parent only: a symlink member is transported as a link,
	// including a dangling link, never dereferenced into its target's data.
	return managedPathContains(scope.namespace, filepath.Join(cached.path, filepath.Base(full))), nil
}

func (scope *managedSyncScope) filter(paths []string) ([]string, error) {
	if scope.namespace == "" {
		return paths, nil
	}
	out := make([]string, 0, len(paths))
	for _, rel := range paths {
		protected, err := scope.contains(rel)
		if err != nil {
			return nil, err
		}
		if !protected {
			out = append(out, rel)
		}
	}
	return out, nil
}
