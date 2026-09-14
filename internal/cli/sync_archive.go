package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func CreateSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest, tempPattern string) (*os.File, error) {
	managed, err := newManagedSyncScope(repo.Root)
	if err != nil {
		return nil, err
	}
	for _, rel := range manifest.Files {
		protected, err := managed.contains(rel)
		if err != nil {
			return nil, err
		}
		if protected {
			return nil, Exit(6, "sync archive contains a protected managed-state member")
		}
	}
	archive, err := os.CreateTemp("", tempPattern)
	if err != nil {
		return nil, fmt.Errorf("create sync archive temp file: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			name := archive.Name()
			_ = archive.Close()
			_ = os.Remove(name)
		}
	}()
	gz := gzip.NewWriter(archive)
	tw := tar.NewWriter(gz)
	for _, rel := range manifest.Files {
		protected, scopeErr := managed.contains(rel)
		if scopeErr != nil || protected {
			_ = tw.Close()
			_ = gz.Close()
			return nil, Exit(6, "sync archive managed-state scope changed before member read: %v", scopeErr)
		}
		if err := appendSyncArchiveMember(ctx, tw, repo.Root, rel); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return nil, Exit(6, "create sync archive: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return nil, Exit(6, "create sync archive: %v", err)
	}
	if err := gz.Close(); err != nil {
		return nil, Exit(6, "create sync archive: %v", err)
	}
	if _, err := archive.Seek(0, 0); err != nil {
		return nil, fmt.Errorf("rewind sync archive: %w", err)
	}
	keep = true
	return archive, nil
}

func appendSyncArchiveMember(ctx context.Context, tw *tar.Writer, root, rel string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	clean := path.Clean(filepath.ToSlash(rel))
	if clean == "." || clean != filepath.ToSlash(rel) || path.IsAbs(clean) || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("unsafe sync path %q", rel)
	}
	full := filepath.Join(root, filepath.FromSlash(clean))
	info, err := os.Lstat(full)
	if err != nil {
		return fmt.Errorf("stat sync path %s: %w", rel, err)
	}
	if info.IsDir() {
		return nil
	}
	linkname := ""
	if info.Mode()&os.ModeSymlink != 0 {
		linkname, err = os.Readlink(full)
		if err != nil {
			return fmt.Errorf("read symlink %s: %w", rel, err)
		}
	}
	header, err := tar.FileInfoHeader(info, linkname)
	if err != nil {
		return fmt.Errorf("archive header %s: %w", rel, err)
	}
	header.Name = clean
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("archive header %s: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	file, err := os.Open(full)
	if err != nil {
		return fmt.Errorf("open sync path %s: %w", rel, err)
	}
	defer file.Close()
	if _, err := copySourceBytes(ctx, tw, file, -1); err != nil {
		return fmt.Errorf("archive path %s: %w", rel, err)
	}
	return nil
}
