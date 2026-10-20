//go:build windows

package cli

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

func replaceClaimFile(tmpPath, path string) error {
	return replaceControllerFile(tmpPath, path)
}

func replaceClaimFileWithRetry(ctx context.Context, tmpPath, path string, unchanged func() error) error {
	return retryClaimPublication(ctx, unchanged, waitClaimPublicationRetry, func() (bool, error) {
		return renameWindowsFileWithOps(tmpPath, path, windows.FILE_RENAME_REPLACE_IF_EXISTS|windows.FILE_RENAME_POSIX_SEMANTICS, defaultWindowsRenameOps())
	})
}

func retryClaimPublication(ctx context.Context, unchanged func() error, wait func(context.Context, time.Duration) error, rename func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	delays := [...]time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("claim publication retry cancelled: %w", err)
		}
		retryable, err := rename()
		if err == nil || !retryable {
			return err
		}
		if attempt == len(delays) {
			return fmt.Errorf("claim publication blocked after %d attempts: %w", attempt+1, err)
		}
		if err := wait(ctx, delays[attempt]); err != nil {
			return fmt.Errorf("claim publication retry wait: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("claim publication retry cancelled: %w", err)
		}
		if err := unchanged(); err != nil {
			return err
		}
	}
}

func waitClaimPublicationRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
