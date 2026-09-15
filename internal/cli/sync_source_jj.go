package cli

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// These native metadata facts establish scope eligibility, not an accepted
// content snapshot. Capture and source-coherence checks must still succeed.
type jjSourceInventory struct {
	jjProtocolHeader
	Identity              jjSourceIdentity    `json:"identity"`
	WorkingCopyOperation  string              `json:"working_copy_operation"`
	WorkingCopyFreshness  string              `json:"working_copy_freshness"`
	WorkingCopyTreeIDs    []string            `json:"working_copy_tree_ids"`
	WorkingCopyTreeLabels []string            `json:"working_copy_tree_labels"`
	Pending               []jjPendingEntry    `json:"pending_entries"`
	SparsePrefixes        []string            `json:"sparse_prefixes"`
	Tracked               []jjTrackedEntry    `json:"tracked_entries"`
	AutoTrack             string              `json:"auto_track"`
	MaxNewFileSize        uint64              `json:"max_new_file_size"`
	Admitted              []jjAdmittedFile    `json:"admitted_entries"`
	Observations          []jjPathObservation `json:"observations"`
	Untracked             []jjUntrackedEntry  `json:"untracked"`
	InvalidUTF8PathCount  uint64              `json:"invalid_utf8_path_count"`
	InputUsedBytes        uint64              `json:"input_used_bytes"`
}

type jjTrackedEntry struct {
	Path                 string `json:"path"`
	Kind                 string `json:"kind"`
	MaterializedConflict bool   `json:"materialized_conflict"`
}

type jjUntrackedEntry struct {
	Path   string            `json:"path"`
	Reason jjUntrackedReason `json:"reason"`
}

type jjUntrackedReason struct {
	Kind    string  `json:"kind"`
	Size    *uint64 `json:"size,omitempty"`
	MaxSize *uint64 `json:"max_size,omitempty"`
}

type jjSourceSelection struct {
	Manifest SyncManifest
	Excludes SyncExcludeRules
	Files    []sourceSnapshotFile
}

// Selection binds native admission to filesystem observations before any
// content copy. Native operation/scope revalidation still owns acceptance.
func selectJJSourceFiles(ctx context.Context, root string, cfg Config, inventory jjSourceInventory, force bool, stderr io.Writer) (jjSourceSelection, error) {
	if err := ctx.Err(); err != nil {
		return jjSourceSelection{}, err
	}
	excludes, err := syncExcludes(root, cfg)
	if err != nil {
		return jjSourceSelection{}, err
	}
	manifest, err := syncManifestFilteredRulesWithSource(root, excludes, syncIncludes(cfg), jjSyncManifestSource(inventory))
	if err != nil {
		return jjSourceSelection{}, err
	}
	admitted := make(map[string]jjAdmittedFile, len(inventory.Admitted))
	for _, file := range inventory.Admitted {
		admitted[file.Path] = file
	}
	selection := jjSourceSelection{Manifest: manifest, Excludes: excludes}
	var bytes uint64
	for _, path := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return jjSourceSelection{}, err
		}
		file, ok := admitted[path]
		if !ok {
			return jjSourceSelection{}, fmt.Errorf("selected native source path %q has no admission", path)
		}
		if file.ObservedSize > math.MaxInt64-bytes {
			return jjSourceSelection{}, fmt.Errorf("selected native source exceeds supported byte accounting")
		}
		bytes += file.ObservedSize
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return jjSourceSelection{}, err
		}
		kindMatches := file.Kind == "file" && info.Mode().IsRegular() || file.Kind == "symlink" && info.Mode()&os.ModeSymlink != 0
		if !kindMatches || info.Size() < 0 || uint64(info.Size()) != file.ObservedSize || info.ModTime().UnixMilli() != file.ObservedMtimeMillis {
			return jjSourceSelection{}, fmt.Errorf("%w: native source observation changed at %q", errSourceSnapshotDrift, path)
		}
		if runtime.GOOS != "windows" && file.Kind == "file" && file.Executable != nil && *file.Executable != (info.Mode().Perm()&0o111 != 0) {
			return jjSourceSelection{}, fmt.Errorf("%w: native source executable mode changed at %q", errSourceSnapshotDrift, path)
		}
		selection.Files = append(selection.Files, sourceSnapshotFile{Path: path, Observed: info})
	}
	if manifest.Bytes != int64(bytes) {
		return jjSourceSelection{}, fmt.Errorf("%w: selected native source size changed", errSourceSnapshotDrift)
	}
	if err := checkSyncPreflight(FullSyncGuardrailManifest(manifest), cfg, force, stderr); err != nil {
		return jjSourceSelection{}, err
	}
	return selection, ctx.Err()
}

type jjPendingEntry struct {
	Path                  string `json:"path"`
	Kind                  string `json:"kind"`
	CoversDescendants     bool   `json:"covers_descendants"`
	TrackedRegular        bool   `json:"tracked_regular"`
	InSparseScope         bool   `json:"in_sparse_scope"`
	CachedMaterialization string `json:"cached_materialization"`
}

type jjAdmittedFile struct {
	TrackedInWorkingCopy bool   `json:"tracked_in_working_copy"`
	Path                 string `json:"path"`
	Kind                 string `json:"kind"`
	Executable           *bool  `json:"executable"`
	ObservedSize         uint64 `json:"observed_size"`
	ObservedMtimeMillis  int64  `json:"observed_mtime_ms"`
}

type jjPathObservation struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Subtree bool   `json:"subtree"`
}

type jjScopeOmission struct {
	Path, Reason string
}

type jjIncompleteSourceError struct {
	Omissions []jjScopeOmission
}

func jjSyncManifestSource(inventory jjSourceInventory) syncManifestSource {
	observations := indexJJObservations(inventory.Observations)
	return syncManifestSource{
		fileList: func(string) ([]byte, error) {
			paths := make([]string, 0, len(inventory.Admitted))
			for _, file := range inventory.Admitted {
				if jjRepositoryMetadataPath(file.Path) {
					continue
				}
				paths = append(paths, file.Path)
			}
			return []byte(strings.Join(paths, "\x00")), nil
		},
		scope: func(root string, excludes SyncExcludeRules, includes []string) (syncManifestScope, error) {
			return validatedJJSyncManifestScope(root, excludes, includes, inventory)
		},
		deleted: func(_ string, excludes SyncExcludeRules, includes []string, trackedRegular map[string]struct{}) ([]string, map[string]struct{}, map[string]struct{}, error) {
			var deleted []string
			regular := map[string]struct{}{}
			for _, entry := range inventory.Pending {
				kind := observations.forPath(entry.Path)
				_, tracked := trackedRegular[entry.Path]
				if (kind != "removed_absent" && kind != "removed_kind_replacement") || entry.Kind == "submodule" || jjRepositoryMetadataPath(entry.Path) ||
					!pathIncluded(entry.Path, includes) || pathExcludedByRules(entry.Path, excludes, tracked) {
					continue
				}
				deleted = append(deleted, entry.Path)
				if tracked {
					regular[entry.Path] = struct{}{}
				}
			}
			sort.Strings(deleted)
			return deleted, nil, regular, nil
		},
		changed: func(_ string, _ SyncExcludeRules, _ []string, _ map[string]struct{}, manifest SyncManifest) ([]string, error) {
			// Native plain-file transfer has no Git dirty-delta dependency.
			return manifest.Files, nil
		},
	}
}

func (e *jjIncompleteSourceError) Error() string {
	items := make([]string, 0, min(len(e.Omissions), 8))
	for _, omission := range e.Omissions[:min(len(e.Omissions), 8)] {
		items = append(items, fmt.Sprintf("%q (%s)", omission.Path, omission.Reason))
	}
	return fmt.Sprintf("native JJ source has %d required incomplete paths: %s; materialize the selected content or adjust sync.include and ordered sync.exclude rules", len(e.Omissions), strings.Join(items, ", "))
}

// Scope is checked before projection: an omitted required path would otherwise
// disappear from the full manifest and become an implicit remote deletion.
func validatedJJSyncManifestScope(root string, excludes SyncExcludeRules, includes []string, inventory jjSourceInventory) (syncManifestScope, error) {
	managed, excludes, err := prepareSyncManifestRoot(root, excludes)
	if err != nil {
		return syncManifestScope{}, err
	}
	scope := syncManifestScope{trackedRegular: map[string]struct{}{}, gitlinkPaths: map[string]struct{}{}}
	observations := indexJJObservations(inventory.Observations)
	admitted := make(map[string]bool, len(inventory.Admitted))
	for _, file := range inventory.Admitted {
		admitted[file.Path] = true
	}
	var omissions []jjScopeOmission
	for _, entry := range inventory.Pending {
		if !safeRepoRel(entry.Path) {
			return syncManifestScope{}, fmt.Errorf("invalid native JJ pending path %q", entry.Path)
		}
		if jjRepositoryMetadataPath(entry.Path) {
			continue
		}
		if entry.TrackedRegular {
			scope.trackedRegular[entry.Path] = struct{}{}
		}
		if entry.Kind == "submodule" {
			scope.gitlinkPaths[entry.Path] = struct{}{}
		}
		protected, err := managed.contains(entry.Path)
		if err != nil {
			return syncManifestScope{}, err
		}
		if protected {
			continue
		}
		direct := pathIncluded(entry.Path, includes) && !pathExcludedByRules(entry.Path, excludes, entry.TrackedRegular)
		descendants := jjSelectedDescendants(entry.Path, entry.CoversDescendants || entry.Kind == "submodule", direct, includes, excludes)
		if descendants {
			omissions = append(omissions, jjScopeOmission{entry.Path, "selected descendants are not materialized"})
			continue
		}
		if !direct || entry.Kind == "submodule" {
			continue
		}
		reason := ""
		switch {
		case !entry.InSparseScope:
			reason = "outside native sparse scope"
		case admitted[entry.Path]:
			continue
		default:
			observation := observations.forPath(entry.Path)
			if observation == "removed_absent" || observation == "removed_kind_replacement" {
				if entry.CachedMaterialization == "cached" {
					continue
				}
				reason = "prior materialization is unestablished"
			} else if observation != "" {
				reason = observation
			} else {
				reason = "required path was not admitted"
			}
		}
		omissions = append(omissions, jjScopeOmission{entry.Path, reason})
	}
	if len(omissions) != 0 {
		return syncManifestScope{}, &jjIncompleteSourceError{Omissions: omissions}
	}
	return scope, nil
}

func jjRepositoryMetadataPath(path string) bool {
	for _, component := range strings.Split(path, "/") {
		if strings.EqualFold(component, ".git") || strings.EqualFold(component, ".jj") {
			return true
		}
	}
	return false
}

type jjObservationIndex struct {
	exact, subtrees map[string]string
}

func indexJJObservations(observations []jjPathObservation) jjObservationIndex {
	index := jjObservationIndex{exact: map[string]string{}, subtrees: map[string]string{}}
	for _, observation := range observations {
		index.exact[observation.Path] = preferredJJObservation(index.exact[observation.Path], observation.Kind)
		if observation.Subtree {
			index.subtrees[observation.Path] = preferredJJObservation(index.subtrees[observation.Path], observation.Kind)
		}
	}
	return index
}

func (index jjObservationIndex) forPath(path string) string {
	result := index.exact[path]
	for {
		cut := strings.LastIndexByte(path, '/')
		if cut < 0 {
			return result
		}
		path = path[:cut]
		result = preferredJJObservation(result, index.subtrees[path])
	}
}

func preferredJJObservation(current, candidate string) string {
	if current == "" || !strings.HasPrefix(current, "omitted_") && strings.HasPrefix(candidate, "omitted_") {
		return candidate
	}
	return current
}

func jjSelectedDescendants(path string, boundary, direct bool, includes []string, excludes SyncExcludeRules) bool {
	// Empty includes select the boundary itself, not hydration below it.
	if len(includes) == 0 {
		includes = []string{path}
	}
	return boundary && !direct && directorySyncNestedRepositoryInScope(path, includes, excludes)
}
