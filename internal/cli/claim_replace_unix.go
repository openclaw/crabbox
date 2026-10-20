//go:build !windows

package cli

import (
	"context"
	"os"
)

func replaceClaimFile(tmpPath, path string) error {
	return os.Rename(tmpPath, path)
}

func replaceClaimFileWithRetry(_ context.Context, tmpPath, path string, _ func() error) error {
	return replaceClaimFile(tmpPath, path)
}
