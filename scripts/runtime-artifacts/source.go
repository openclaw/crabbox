package main

import (
	"context"
	"fmt"
	"os"

	"github.com/openclaw/crabbox/internal/runner"
)

// sourceID reads candidate bytes with the protected tool's fingerprint algorithm.
// Root confines both directory traversal and subsequent file opens, including
// symlinks, without executing any code from the supplied source tree.
func sourceID(ctx context.Context, sourceDirectory string) (string, error) {
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	if sourceDirectory == "" {
		return "", fmt.Errorf("source directory is required")
	}
	root, err := os.OpenRoot(sourceDirectory)
	if err != nil {
		return "", err
	}
	defer root.Close()
	source, err := root.OpenRoot("internal/runner")
	if err != nil {
		return "", err
	}
	defer source.Close()
	id, err := runner.SourceIDFromFS(source.FS())
	if err != nil {
		return "", err
	}
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	return id, nil
}
