package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Capture the inputs to a manifest, not another projected/stat-sized manifest.
// Two equal captures bracket copying and the final uncached content read.
type localGitSnapshotInputs struct {
	head, root, ignore string
	sparse             bool
	index, files       []byte
	deleted, cached    []byte
}

func captureLocalGitSnapshotInputs(ctx context.Context, root string) (inputs localGitSnapshotInputs, err error) {
	// Root discovery is checked on both sides so an enclosing/replaced repository
	// cannot supply the same HEAD/index while changing the workspace boundary.
	inputs.root, err = localGitSeedSourceOutput(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		return inputs, err
	}
	if !sameRepositoryPath(inputs.root, root) {
		return inputs, fmt.Errorf("checkout_root_mismatch")
	}
	inputs.sparse, err = localGitSnapshotSparseEnabled(ctx, root)
	if err != nil {
		return inputs, err
	}
	inputs.ignore, err = localGitSnapshotGlobalIgnore(ctx, root)
	if err != nil {
		return inputs, err
	}
	inputs.files, err = localGitSnapshotBytes(ctx, root, nil, "-c", "core.excludesFile="+inputs.ignore, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return inputs, err
	}
	inputs.deleted, err = localGitSnapshotBytes(ctx, root, nil, "ls-files", "--deleted", "-z")
	if err != nil {
		return inputs, err
	}
	inputs.cached, err = localGitSnapshotBytes(ctx, root, nil, "diff", "--cached", "--raw", "--format=", "-z", "--diff-filter=D", "--no-renames", "--no-ext-diff", "--no-textconv")
	if err != nil {
		return inputs, err
	}
	// Close each capture with the index and HEAD, rather than sampling them
	// before potentially lengthy enumeration of working files.
	inputs.index, err = localGitSnapshotBytes(ctx, root, nil, "ls-files", "-v", "--stage", "-z")
	if err != nil {
		return inputs, err
	}
	inputs.head, err = localGitSeedSourceOutput(ctx, root, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil || !validGitObjectID(inputs.head) {
		return inputs, errors.Join(fmt.Errorf("invalid_head"), err, ctx.Err())
	}
	return inputs, err
}

func (inputs localGitSnapshotInputs) same(other localGitSnapshotInputs) bool {
	return inputs.head == other.head && inputs.root == other.root && inputs.ignore == other.ignore && inputs.sparse == other.sparse &&
		bytes.Equal(inputs.index, other.index) && bytes.Equal(inputs.files, other.files) &&
		bytes.Equal(inputs.deleted, other.deleted) && bytes.Equal(inputs.cached, other.cached)
}

func (inputs localGitSnapshotInputs) checkout() gitOverlayCheckoutState {
	return gitOverlayCheckoutState{Head: inputs.head, IndexFingerprint: fmt.Sprintf("%x", sha256.Sum256(inputs.index))}
}

func (inputs localGitSnapshotInputs) validate(ctx context.Context, root string) ([]gitTrackedPath, error) {
	tracked, err := parseGitTrackedPaths(inputs.index)
	if err != nil {
		return nil, err
	}
	for _, entry := range tracked {
		if entry.stage != 0 {
			return nil, fmt.Errorf("unmerged_index")
		}
		if entry.assumeUnchanged {
			return nil, fmt.Errorf("assume_unchanged_index")
		}
		if entry.mode == "160000" {
			return nil, fmt.Errorf("gitlink")
		}
	}
	hidden, err := gitCheckoutHiddenOmissionForTracked(root, tracked, inputs.sparse, nil, localGitSnapshotSparseRules(ctx))
	if err != nil {
		return nil, err
	}
	if hidden != "" {
		return nil, fmt.Errorf("sparse_hidden_path")
	}
	return tracked, nil
}

func (inputs localGitSnapshotInputs) manifest(ctx context.Context, root string, excludes SyncExcludeRules, includes []string) (SyncManifest, error) {
	tracked, err := inputs.validate(ctx, root)
	if err != nil {
		return SyncManifest{}, err
	}
	managed, excludes, err := prepareSyncManifestRoot(root, excludes)
	if err != nil {
		return SyncManifest{}, err
	}
	scope := syncManifestScope{trackedRegular: trackedRegularPathSet(tracked), gitlinkPaths: map[string]struct{}{}}
	manifest, seen, err := projectSyncManifest(root, excludes, includes, splitNul(inputs.files), scope, managed)
	if err != nil {
		return SyncManifest{}, err
	}
	cached, err := parseGitCachedDeletions(inputs.cached)
	if err != nil {
		return SyncManifest{}, err
	}
	deleted, gitlinks, _, err := projectSyncDeletedPaths(excludes, includes, scope.trackedRegular, inputs.deleted, cached)
	if err != nil {
		return SyncManifest{}, err
	}
	deleted, err = managed.filter(deleted)
	if err != nil {
		return SyncManifest{}, err
	}
	manifest.Deleted = filterDeletedPaths(deleted, seen, gitlinks)
	manifest.Changed = append(slices.Clone(manifest.Files), manifest.Deleted...)
	slices.Sort(manifest.Changed)
	// Every selected working file is transported; deleted paths have no bytes.
	// Reuse the sizes just observed during projection instead of two more stats.
	manifest.ChangedBytes = manifest.Bytes
	manifest.OverlayFiles = slices.Clone(manifest.Files)
	manifest.OverlayBytes = manifest.Bytes
	return manifest, ctx.Err()
}

func validateLocalGitSnapshotTree(ctx context.Context, root, head string) error {
	// A selected commit is immutable: check historical gitlinks once per attempt.
	tree, err := localGitSnapshotBytes(ctx, root, nil, "ls-tree", "-r", "-z", "--full-tree", head)
	if err != nil {
		return fmt.Errorf("head_tree: %w", err)
	}
	for _, entry := range bytes.Split(tree, []byte{0}) {
		if strings.HasPrefix(string(entry), "160000 ") {
			return fmt.Errorf("gitlink")
		}
	}
	return nil
}
