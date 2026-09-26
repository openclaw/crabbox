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
	"strings"
)

// Local seeds transport every accepted working file independently of Git's
// checkout transforms; only the snapshot acquisition loop is shared with overlay.
func prepareLocalGitSeedSnapshot(ctx context.Context, repo Repo, cfg Config, _ SyncExcludeRules) (gitOverlaySnapshot, error) {
	return prepareLocalGitSeedSnapshotWithHook(ctx, repo, cfg, nil)
}

func prepareLocalGitSeedSnapshotWithHook(ctx context.Context, repo Repo, cfg Config, hook sourceSnapshotHook) (gitOverlaySnapshot, error) {
	for attempt := 1; attempt <= gitOverlaySnapshotMaxAttempts; attempt++ {
		snapshot, err := prepareLocalGitSeedSnapshotAttempt(withSyncDigests(ctx), repo, cfg, attempt, hook)
		if err == nil {
			return snapshot, nil
		}
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		cleanupErr := snapshot.cleanup()
		if cleanupErr != nil {
			return snapshot, errors.Join(err, cleanupErr)
		}
		if !errors.Is(err, errSourceSnapshotDrift) || ctx.Err() != nil {
			return gitOverlaySnapshot{}, err
		}
	}
	return gitOverlaySnapshot{}, errSourceSnapshotDrift
}

func prepareLocalGitSeedSnapshotAttempt(ctx context.Context, repo Repo, cfg Config, attempt int, hook sourceSnapshotHook) (snapshot gitOverlaySnapshot, err error) {
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	inputs, err := captureLocalGitSnapshotInputs(ctx, repo.Root)
	if err != nil {
		return snapshot, err
	}
	if inputs.head != repo.Head {
		return snapshot, fmt.Errorf("head_changed")
	}
	excludes, err := syncExcludes(repo.Root, cfg)
	if err != nil {
		return snapshot, err
	}
	managed, excludes, err := prepareSyncManifestRoot(repo.Root, excludes)
	if err != nil {
		return snapshot, err
	}
	manifest, err := inputs.manifest(ctx, repo.Root, excludes, syncIncludes(cfg))
	if err != nil {
		return snapshot, err
	}
	if err := validateLocalGitSnapshotTree(ctx, repo.Root, inputs.head); err != nil {
		return snapshot, err
	}
	if hook != nil {
		hook("manifest_built", attempt, "")
	}
	snapshot, err = newGitOverlaySnapshot()
	if err != nil {
		return snapshot, err
	}
	snapshot.Manifest, snapshot.Excludes, snapshot.Checkout = manifest, excludes, inputs.checkout()
	if hook != nil {
		hook("snapshot_created", attempt, snapshot.Root)
	}
	if err := copySourceSnapshotOwned(ctx, repo.Root, &snapshot.sourceSnapshot, manifest.Files, attempt, hook); err != nil {
		return snapshot, err
	}
	if hook != nil {
		hook("snapshot_copied", attempt, snapshot.Root)
	}
	frozenRepo := repo
	frozenRepo.Root = snapshot.Root
	snapshot.Fingerprint, err = localGitSeedSnapshotFingerprint(ctx, frozenRepo, cfg, manifest, excludes, snapshot.Checkout)
	if err != nil {
		return snapshot, err
	}
	if hook != nil {
		hook("snapshot_fingerprinted", attempt, snapshot.Root)
	}
	live, err := localGitSeedSnapshotFingerprint(ctx, repo, cfg, manifest, excludes, snapshot.Checkout)
	if err != nil {
		if os.IsNotExist(err) {
			err = errors.Join(errSourceSnapshotDrift, err)
		}
		return snapshot, err
	}
	if hook != nil {
		hook("live_fingerprinted", attempt, snapshot.Root)
	}
	// Verify path confinement again; unchanged file bytes do not establish that
	// an ancestor still belongs to this checkout rather than a symlink target.
	for _, rel := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return snapshot, err
		}
		if err := validateGitOverlayPath(repo.Root, rel); err != nil {
			return snapshot, fmt.Errorf("%w: %v", errSourceSnapshotDrift, err)
		}
	}
	// Dirty mmap pages can change without advancing either timestamp. Acceptance
	// requires current bytes, independently of the digests retained during copy.
	acceptanceCtx := context.WithValue(ctx, syncDigestContextKey{}, (*syncDigestCache)(nil))
	accepted, acceptedBytes, err := localGitSeedSnapshotFingerprintAndSize(acceptanceCtx, repo, cfg, manifest, excludes, snapshot.Checkout)
	if err != nil {
		if os.IsNotExist(err) {
			err = errors.Join(errSourceSnapshotDrift, err)
		}
		return snapshot, err
	}
	if hook != nil {
		hook("before_acceptance_inputs", attempt, snapshot.Root)
	}
	// One closing input capture replaces repeated manifest construction and
	// double-sampled checkout calls. No captured input may change across the
	// copy and uncached read; new files and deletions cannot hide behind sizes.
	finalExcludes, err := syncExcludes(repo.Root, cfg)
	if err != nil {
		return snapshot, err
	}
	final, err := captureLocalGitSnapshotInputs(ctx, repo.Root)
	if err != nil {
		return snapshot, err
	}
	finalManaged, finalExcludes, err := prepareSyncManifestRoot(repo.Root, finalExcludes)
	if err != nil {
		return snapshot, err
	}
	if !inputs.same(final) || !sameSyncExcludeRules(excludes, finalExcludes) ||
		managed.source != finalManaged.source || managed.namespace != finalManaged.namespace ||
		live != snapshot.Fingerprint || accepted != snapshot.Fingerprint || acceptedBytes != manifest.Bytes {
		return snapshot, errSourceSnapshotDrift
	}
	// Sparse state can hide paths outside the selected manifest as well. The
	// immutable captured index is reused, but missing-path checks remain live.
	if _, err := final.validate(ctx, repo.Root); err != nil {
		return snapshot, err
	}
	return snapshot, ctx.Err()
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
	fingerprint, _, err := localGitSeedSnapshotFingerprintAndSize(ctx, repo, cfg, manifest, excludes, checkout)
	return fingerprint, err
}

func localGitSeedSnapshotFingerprintAndSize(ctx context.Context, repo Repo, cfg Config, manifest SyncManifest, excludes SyncExcludeRules, checkout gitOverlayCheckoutState) (string, int64, error) {
	h := sha256.New()
	fmt.Fprintf(h, "v2-local-git-snapshot\nhead=%s\nindex=%s\n", checkout.Head, checkout.IndexFingerprint)
	fmt.Fprintf(h, "delete=%t\nchecksum=%t\n", cfg.Sync.Delete, cfg.Sync.Checksum)
	fmt.Fprintf(h, "manifest=%x\ndeleted=%x\n", sha256.Sum256(manifest.NUL()), sha256.Sum256(manifest.DeletedNUL()))
	for _, include := range syncIncludes(cfg) {
		fmt.Fprintf(h, "include=%q\n", include)
	}
	for _, exclude := range excludes.rules {
		fmt.Fprintf(h, "exclude=%d:%q\n", exclude.origin, exclude.pattern)
	}
	fmt.Fprintf(h, "managedSubtree=%q\n", excludes.managedSubtree)
	size, err := syncFingerprintPathsAndSize(ctx, h, repo.Root, manifest.Files, true, true)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}
