package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Local seeds transport every accepted working file independently of Git's
// checkout transforms; only the snapshot acquisition loop is shared with overlay.
func prepareLocalGitSeedSnapshot(ctx context.Context, repo Repo, cfg Config, _ SyncExcludeRules) (gitOverlaySnapshot, error) {
	return prepareLocalGitSeedSnapshotWithHook(ctx, repo, cfg, nil)
}

func prepareLocalGitSeedSnapshotWithHook(ctx context.Context, repo Repo, cfg Config, hook sourceSnapshotHook) (gitOverlaySnapshot, error) {
	policy := gitSnapshotPolicy{
		target: repo.Head,
		checkout: func(root string) (gitOverlayCheckoutState, error) {
			return captureGitCheckoutStateWithStateReader(root, nil, func(root string, args ...string) ([]byte, error) {
				return localGitSnapshotBytes(ctx, root, nil, args...)
			}, func(root string) (gitOverlayCheckoutState, error) {
				return readLocalGitSeedCheckoutState(ctx, root)
			})
		},
		manifest: func(root string, excludes SyncExcludeRules, includes []string) (SyncManifest, error) {
			return localGitSnapshotManifest(ctx, root, excludes, includes)
		},
		validate: func(repo Repo, manifest SyncManifest, checkout gitOverlayCheckoutState) error {
			return validateLocalGitSeedManifestAtState(ctx, repo, manifest, checkout)
		},
		files: func(manifest SyncManifest) []string { return manifest.Files },
		fingerprint: func(repo Repo, manifest SyncManifest, excludes SyncExcludeRules, checkout gitOverlayCheckoutState) (string, error) {
			return localGitSeedSnapshotFingerprint(ctx, repo, cfg, manifest, excludes, checkout)
		},
	}
	return prepareGitSnapshotWithCleanup(ctx, repo, cfg, syncIncludes(cfg), policy, hook, func(snapshot *gitOverlaySnapshot) error {
		return snapshot.cleanup()
	})
}

func readLocalGitSeedCheckoutState(ctx context.Context, root string) (gitOverlayCheckoutState, error) {
	head, err := localGitSeedSourceOutput(ctx, root, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil && (ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return gitOverlayCheckoutState{}, errors.Join(err, ctx.Err())
	}
	if err != nil || !validGitObjectID(head) {
		return gitOverlayCheckoutState{}, fmt.Errorf("invalid_head")
	}
	index, err := localGitSnapshotBytes(ctx, root, nil, "ls-files", "-v", "--stage", "-z")
	if err != nil {
		return gitOverlayCheckoutState{}, err
	}
	// Hash the index's semantic entries without write-tree, which writes both
	// cache-tree extensions and new tree objects into the source repository.
	return gitOverlayCheckoutState{Head: head, IndexFingerprint: fmt.Sprintf("%x", sha256.Sum256(index))}, nil
}

func validateLocalGitSeedManifestAtState(ctx context.Context, repo Repo, manifest SyncManifest, checkout gitOverlayCheckoutState) error {
	if repo.Root == "" || repo.Head == "" {
		return fmt.Errorf("missing_repo_identity")
	}
	if checkout.Head != repo.Head {
		return fmt.Errorf("head_changed")
	}
	tracked, err := localGitSnapshotTracked(ctx, repo.Root)
	if err != nil {
		return err
	}
	for _, entry := range tracked {
		if entry.stage != 0 {
			return fmt.Errorf("unmerged_index")
		}
		if entry.assumeUnchanged {
			return fmt.Errorf("assume_unchanged_index")
		}
		if entry.mode == "160000" {
			return fmt.Errorf("gitlink")
		}
	}
	sparse, err := localGitSnapshotSparseEnabled(ctx, repo.Root)
	if err != nil {
		return err
	}
	hidden, err := gitCheckoutHiddenOmissionForTracked(repo.Root, tracked, sparse, nil, localGitSnapshotSparseRules(ctx))
	if err != nil {
		return err
	}
	if hidden != "" {
		return fmt.Errorf("sparse_hidden_path")
	}
	tree, err := localGitSnapshotBytes(ctx, repo.Root, nil, "ls-tree", "-r", "-z", "--full-tree", checkout.Head)
	if err != nil {
		return fmt.Errorf("head_tree: %w", err)
	}
	for _, entry := range bytes.Split(tree, []byte{0}) {
		if bytes.HasPrefix(entry, []byte("160000 ")) {
			return fmt.Errorf("gitlink")
		}
	}
	for _, rel := range manifest.Files {
		if err := validateGitOverlayPath(repo.Root, rel); err != nil {
			return err
		}
	}
	return nil
}

func localGitSnapshotBytes(ctx context.Context, root string, input []byte, args ...string) ([]byte, error) {
	var out bytes.Buffer
	cmd := localGitSeedCommand(ctx, root, true, args...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = &boundedGitSeedWriter{dst: &out, remaining: localGitSeedMaxMetadata}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("local Git snapshot %s failed: %w", args[0], err)
	}
	return out.Bytes(), nil
}

func localGitSnapshotTracked(ctx context.Context, root string) ([]gitTrackedPath, error) {
	tagged, err := localGitSnapshotBytes(ctx, root, nil, "ls-files", "-v", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	return parseGitTrackedPaths(tagged)
}

func localGitSnapshotSparseEnabled(ctx context.Context, root string) (bool, error) {
	out, err := localGitSnapshotBytes(ctx, root, nil, "config", "--bool", "--get", "core.sparseCheckout")
	if err != nil && exitCode(err) != 1 {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

func localGitSnapshotSparseRules(ctx context.Context) func(string, []gitTrackedPath) (map[string]struct{}, error) {
	return func(root string, tracked []gitTrackedPath) (map[string]struct{}, error) {
		var paths bytes.Buffer
		for _, entry := range tracked {
			paths.WriteString(entry.name)
			paths.WriteByte(0)
		}
		out, err := localGitSnapshotBytes(ctx, root, paths.Bytes(), "sparse-checkout", "check-rules", "-z")
		if err != nil {
			return nil, fmt.Errorf("git sparse-checkout check-rules unavailable: %w", err)
		}
		return nulPathSet(out), nil
	}
}

func localGitSnapshotManifest(ctx context.Context, root string, excludes SyncExcludeRules, includes []string) (SyncManifest, error) {
	ignoreFile, err := localGitSnapshotGlobalIgnore(ctx, root)
	if err != nil {
		return SyncManifest{}, err
	}
	return syncManifestFilteredRulesWithSource(root, excludes, includes, syncManifestSource{
		fileList: func(root string) ([]byte, error) {
			return localGitSnapshotBytes(ctx, root, nil, "-c", "core.excludesFile="+ignoreFile, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
		},
		scope: func(root string, excludes SyncExcludeRules, includes []string) (syncManifestScope, error) {
			tracked, err := localGitSnapshotTracked(ctx, root)
			if err != nil {
				return syncManifestScope{}, err
			}
			sparse, err := localGitSnapshotSparseEnabled(ctx, root)
			if err != nil {
				return syncManifestScope{}, err
			}
			return validatedSyncManifestScopeWithTracked(root, excludes, includes, tracked, sparse, localGitSnapshotSparseRules(ctx))
		},
		deleted: func(root string, excludes SyncExcludeRules, includes []string, trackedRegular map[string]struct{}) ([]string, map[string]struct{}, map[string]struct{}, error) {
			worktree, err := localGitSnapshotBytes(ctx, root, nil, "ls-files", "--deleted", "-z")
			if err != nil {
				return nil, nil, nil, err
			}
			raw, err := localGitSnapshotBytes(ctx, root, nil, "diff", "--cached", "--raw", "--format=", "-z", "--diff-filter=D", "--no-renames", "--no-ext-diff", "--no-textconv")
			if err != nil {
				return nil, nil, nil, err
			}
			cached, err := parseGitCachedDeletions(raw)
			if err != nil {
				return nil, nil, nil, err
			}
			return projectSyncDeletedPaths(excludes, includes, trackedRegular, worktree, cached)
		},
		changed: func(_ string, _ SyncExcludeRules, _ []string, _ map[string]struct{}, manifest SyncManifest) ([]string, error) {
			// Full byte transport needs no filter-sensitive worktree diff.
			return append(slices.Clone(manifest.Files), manifest.Deleted...), nil
		},
	})
}

// Read only the ignore-file setting with ordinary config discovery. Disabling
// global configuration for executable Git operations must not expose files the
// user's normal global ignore excludes from the manifest.
func localGitSnapshotGlobalIgnore(ctx context.Context, root string) (string, error) {
	cmd := localGitSeedCommand(ctx, root, true, "config", "--path", "--get", "core.excludesFile")
	cmd.Env = repositoryGitEnvironment()
	var out bytes.Buffer
	cmd.Stdout = &boundedGitSeedWriter{dst: &out, remaining: localGitSeedMaxMetadata}
	err := cmd.Run()
	if err != nil && exitCode(err) != 1 {
		return "", fmt.Errorf("read local Git ignore-file setting: %w", err)
	}
	if err == nil {
		return strings.TrimSpace(out.String()), nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "git", "ignore"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "git", "ignore"), nil
}

func localGitSeedSnapshotFingerprint(ctx context.Context, repo Repo, cfg Config, manifest SyncManifest, excludes SyncExcludeRules, checkout gitOverlayCheckoutState) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "v1-local-git-snapshot\nhead=%s\nindex=%s\n", checkout.Head, checkout.IndexFingerprint)
	fmt.Fprintf(h, "delete=%t\nchecksum=%t\n", cfg.Sync.Delete, cfg.Sync.Checksum)
	fmt.Fprintf(h, "manifest=%x\ndeleted=%x\n", sha256.Sum256(manifest.NUL()), sha256.Sum256(manifest.DeletedNUL()))
	for _, include := range syncIncludes(cfg) {
		fmt.Fprintf(h, "include=%q\n", include)
	}
	for _, exclude := range excludes.rules {
		fmt.Fprintf(h, "exclude=%d:%q\n", exclude.origin, exclude.pattern)
	}
	fmt.Fprintf(h, "managedSubtree=%q\n", excludes.managedSubtree)
	if err := syncFingerprintPaths(ctx, h, repo.Root, manifest.Files, true); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
