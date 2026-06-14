package firecracker

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func prepareWritableRootFS(sourcePath, destPath string, requestedMiB int) error {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return fmt.Errorf("stat firecracker rootfs %s: %w", sourcePath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("firecracker rootfs %s must be a file", sourcePath)
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return fmt.Errorf("create firecracker rootfs directory %s: %w", filepath.Dir(destPath), err)
	}
	if err := copyFile(sourcePath, destPath, 0o600); err != nil {
		return err
	}
	if requestedMiB > 0 {
		targetSize := int64(requestedMiB) * 1024 * 1024
		if targetSize > info.Size() {
			if err := os.Truncate(destPath, targetSize); err != nil {
				return fmt.Errorf("expand firecracker rootfs %s to %d bytes: %w", destPath, targetSize, err)
			}
		}
	}
	return nil
}

func copyFile(sourcePath, destPath string, mode os.FileMode) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open firecracker rootfs %s: %w", sourcePath, err)
	}
	defer source.Close()

	dest, err := os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create firecracker rootfs copy %s: %w", destPath, err)
	}
	defer dest.Close()

	if _, err := io.Copy(dest, source); err != nil {
		return fmt.Errorf("copy firecracker rootfs to %s: %w", destPath, err)
	}
	if err := dest.Sync(); err != nil {
		return fmt.Errorf("sync firecracker rootfs copy %s: %w", destPath, err)
	}
	return nil
}
