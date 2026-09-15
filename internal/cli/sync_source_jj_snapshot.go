package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type jjSourceReader func(context.Context) (jjSourceRead, error)

type jjSourceSnapshot struct {
	sourceSnapshot
	Read          jjSourceRead
	Selection     jjSourceSelection
	ContentDigest string
}

func prepareJJSourceSnapshot(ctx context.Context, root string, cfg Config, reader jjSourceReader, force bool, stderr io.Writer) (jjSourceSnapshot, error) {
	for attempt := 0; attempt < 3; attempt++ {
		snapshot, err := captureJJSourceSnapshot(ctx, root, cfg, reader, force, stderr)
		if err == nil || ctx.Err() != nil || snapshot.Root != "" || !errors.Is(err, errSourceSnapshotDrift) {
			return snapshot, err
		}
	}
	return jjSourceSnapshot{}, errSourceSnapshotDrift
}

func captureJJSourceSnapshot(ctx context.Context, root string, cfg Config, reader jjSourceReader, force bool, stderr io.Writer) (snapshot jjSourceSnapshot, result error) {
	defer func() {
		if result != nil {
			result = errors.Join(result, ctx.Err())
			if snapshot.Root != "" {
				if err := snapshot.cleanup(); err != nil {
					result = errors.Join(result, err)
				} else {
					snapshot = jjSourceSnapshot{}
				}
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	read, err := reader(ctx)
	if err != nil {
		return snapshot, err
	}
	if canonicalRepositoryPath(root) != canonicalRepositoryPath(read.Context.WorkspaceRoot) {
		return snapshot, fmt.Errorf("native inventory belongs to a different source root")
	}
	selection, err := selectJJSourceFiles(ctx, root, cfg, read.Inventory, force, stderr)
	if err != nil {
		return snapshot, err
	}
	tempBase, err := jjSourceTempBase(root, read.Context.RepositoryPath)
	if err != nil {
		return snapshot, err
	}
	snapshot.sourceSnapshot, err = newSourceSnapshotAt(tempBase)
	if err != nil {
		return snapshot, err
	}
	snapshot.Read, snapshot.Selection = read, selection
	if err := copyObservedSourceSnapshotOwned(ctx, root, &snapshot.sourceSnapshot, selection.Files); err != nil {
		return snapshot, err
	}
	snapshot.ContentDigest, err = jjSourceContentDigest(ctx, snapshot.Root, selection.Manifest.Files)
	if err != nil {
		return snapshot, err
	}
	if err := revalidateJJSourceSelection(ctx, root, cfg, snapshot, reader, force); err != nil {
		return snapshot, err
	}
	liveDigest, err := jjSourceContentDigest(ctx, root, selection.Manifest.Files)
	if err != nil {
		return snapshot, err
	}
	if liveDigest != snapshot.ContentDigest {
		return snapshot, errSourceSnapshotDrift
	}
	if err := revalidateJJSourceSelection(ctx, root, cfg, snapshot, reader, force); err != nil {
		return snapshot, err
	}
	return snapshot, ctx.Err()
}

func revalidateJJSourceSelection(ctx context.Context, root string, cfg Config, snapshot jjSourceSnapshot, reader jjSourceReader, force bool) error {
	read, err := reader(ctx)
	if err != nil {
		return err
	}
	if read.MetadataDigest != snapshot.Read.MetadataDigest {
		return errSourceSnapshotDrift
	}
	selection, err := selectJJSourceFiles(ctx, root, cfg, read.Inventory, force, io.Discard)
	if err != nil {
		return err
	}
	if !sameSyncExcludeRules(snapshot.Selection.Excludes, selection.Excludes) ||
		!sameSyncManifest(snapshot.Selection.Manifest, selection.Manifest) || len(snapshot.Selection.Files) != len(selection.Files) {
		return errSourceSnapshotDrift
	}
	for i, file := range snapshot.Selection.Files {
		if file.Path != selection.Files[i].Path || !sameSourceSnapshotIdentity(file.Observed, selection.Files[i].Observed) {
			return errSourceSnapshotDrift
		}
	}
	return ctx.Err()
}

func jjSourceContentDigest(ctx context.Context, root string, paths []string) (string, error) {
	h := sha256.New()
	if err := fingerprintSourcePaths(ctx, h, root, paths, true, true); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func jjSourceTempBase(root, repository string) (string, error) {
	tempBase, err := filepath.Abs(os.TempDir())
	if err != nil {
		return "", err
	}
	tempBase = canonicalRepositoryPath(tempBase)
	for _, excluded := range []string{root, repository} {
		if pathWithinRoot(tempBase, canonicalRepositoryPath(excluded)) {
			return "", fmt.Errorf("native source staging requires a temporary directory outside source and repository administration")
		}
	}
	return tempBase, nil
}
