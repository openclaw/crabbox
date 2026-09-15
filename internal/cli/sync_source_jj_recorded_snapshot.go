package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
)

type jjRecordedSelection struct {
	Paths    []string
	Excludes SyncExcludeRules
	scope    syncManifestScope
	managed  *managedSyncScope
}

type jjRecordedSourceSnapshot struct {
	sourceSnapshot
	nativeState   sourceSnapshot
	Inventory     jjRecordedInventory
	Manifest      SyncManifest
	Excludes      SyncExcludeRules
	ContentDigest string
}

type jjRecordedExportLimits struct{ EntryBytes, OutputBytes uint64 }

func (snapshot *jjRecordedSourceSnapshot) cleanup() error {
	return errors.Join(snapshot.nativeState.cleanup(), snapshot.sourceSnapshot.cleanup())
}

func selectJJRecordedPaths(ctx context.Context, inventory jjRecordedInventory, cfg Config) (jjRecordedSelection, error) {
	root := inventory.Identity.WorkspaceRoot
	excludes, err := syncExcludes(root, cfg)
	if err != nil {
		return jjRecordedSelection{}, err
	}
	managed, excludes, err := prepareSyncManifestRoot(root, excludes)
	if err != nil {
		return jjRecordedSelection{}, err
	}
	selection := jjRecordedSelection{Paths: []string{}, Excludes: excludes, managed: managed, scope: syncManifestScope{trackedRegular: map[string]struct{}{}, gitlinkPaths: map[string]struct{}{}}}
	for _, entry := range inventory.Entries {
		if entry.Kind == "file" || entry.Kind == "file_conflict" {
			selection.scope.trackedRegular[entry.Path] = struct{}{}
		}
		if entry.Kind == "submodule" {
			selection.scope.gitlinkPaths[entry.Path] = struct{}{}
		}
	}
	seen := map[string]bool{}
	for _, entry := range inventory.Entries {
		if err := ctx.Err(); err != nil {
			return jjRecordedSelection{}, err
		}
		if !safeRepoRel(entry.Path) || seen[entry.Path] {
			return jjRecordedSelection{}, fmt.Errorf("invalid or duplicate native recorded path %q", entry.Path)
		}
		seen[entry.Path] = true
		if jjRepositoryMetadataPath(entry.Path) {
			continue
		}
		protected, err := managed.containsRecordedPath(entry.Path)
		if err != nil {
			return jjRecordedSelection{}, err
		}
		if protected {
			continue
		}
		_, regular := selection.scope.trackedRegular[entry.Path]
		direct := pathIncluded(entry.Path, syncIncludes(cfg)) && !pathExcludedByRules(entry.Path, excludes, regular)
		boundary := entry.Kind == "submodule"
		for _, term := range entry.Terms {
			boundary = boundary || term != nil && term.Kind == "tree"
		}
		if jjSelectedDescendants(entry.Path, boundary, direct, syncIncludes(cfg), excludes) {
			return jjRecordedSelection{}, &jjIncompleteSourceError{Omissions: []jjScopeOmission{{Path: entry.Path, Reason: "selected descendants are not represented by a terminal native entry"}}}
		}
		selected, _ := syncManifestPathDecision(entry.Path, excludes, syncIncludes(cfg), selection.scope)
		if selected {
			selection.Paths = append(selection.Paths, entry.Path)
		}
	}
	sort.Strings(selection.Paths)
	return selection, nil
}

func prepareJJRecordedSnapshot(ctx context.Context, process *jjSourceProcess, revision string, cfg Config, limits jjRecordedExportLimits, force bool, stderr io.Writer) (snapshot jjRecordedSourceSnapshot, result error) {
	defer func() {
		if result != nil {
			result = errors.Join(result, ctx.Err())
			if err := snapshot.cleanup(); err != nil {
				result = errors.Join(result, err)
			} else {
				snapshot = jjRecordedSourceSnapshot{}
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	inventory, err := process.readRecorded(ctx, revision)
	if err != nil {
		return snapshot, err
	}
	selection, err := selectJJRecordedPaths(ctx, inventory, cfg)
	if err != nil {
		return snapshot, err
	}
	// Counts are available without fetching blobs; rendered bytes are checked
	// after export, while native hard output limits apply during rendering.
	if err := checkSyncPreflight(FullSyncGuardrailManifest(SyncManifest{Files: selection.Paths}), cfg, force, io.Discard); err != nil {
		return snapshot, err
	}
	tempBase, err := jjSourceTempBase(inventory.Identity.WorkspaceRoot, inventory.Identity.RepositoryPath)
	if err != nil {
		return snapshot, err
	}
	snapshot.sourceSnapshot, err = newSourceSnapshotAt(tempBase)
	if err != nil {
		return snapshot, err
	}
	snapshot.nativeState, err = newSourceSnapshotAt(tempBase)
	if err != nil {
		return snapshot, err
	}
	if _, err := process.exportRecorded(ctx, inventory, selection.Paths, snapshot.Root, snapshot.nativeState.Root, limits.EntryBytes, limits.OutputBytes); err != nil {
		return snapshot, err
	}
	snapshot.Inventory, snapshot.Excludes = inventory, selection.Excludes
	snapshot.Manifest, _, err = projectSyncManifestWithProtection(snapshot.Root, selection.Excludes, syncIncludes(cfg), selection.Paths, selection.scope, selection.managed.containsRecordedPath)
	if err != nil {
		return snapshot, err
	}
	if !slices.Equal(snapshot.Manifest.Files, selection.Paths) {
		return snapshot, fmt.Errorf("native recorded export did not produce every selected manifest member")
	}
	if err := checkSyncPreflight(FullSyncGuardrailManifest(snapshot.Manifest), cfg, force, stderr); err != nil {
		return snapshot, err
	}
	snapshot.ContentDigest, err = jjSourceContentDigest(ctx, snapshot.Root, snapshot.Manifest.Files)
	if err != nil {
		return snapshot, err
	}
	refreshed, err := selectJJRecordedPaths(ctx, inventory, cfg)
	if err != nil {
		return snapshot, err
	}
	if !sameSyncExcludeRules(selection.Excludes, refreshed.Excludes) || !slices.Equal(selection.Paths, refreshed.Paths) {
		return snapshot, fmt.Errorf("%w: recorded source exclusion scope changed", errSourceSnapshotDrift)
	}
	if err := snapshot.nativeState.cleanup(); err != nil {
		return snapshot, err
	}
	return snapshot, ctx.Err()
}
