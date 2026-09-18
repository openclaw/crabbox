package runnerfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// StagedArchive separates validation from publication so a transport can verify
// its terminal frame before changing the destination.
type StagedArchive struct {
	directory string
	target    string
}

func StageArchive(ctx context.Context, archive io.Reader, target, archiveRoot string, options ExtractOptions, limits ArchiveLimits) (*StagedArchive, error) {
	parent := filepath.Dir(target)
	if info, err := os.Stat(parent); err != nil {
		return nil, invalid("copy destination parent is unavailable: %v", err)
	} else if !info.IsDir() {
		return nil, invalid("copy destination parent is not a directory: %s", parent)
	}
	directory, err := os.MkdirTemp(parent, ".crabbox-cp-*")
	if err != nil {
		return nil, fmt.Errorf("create copy extraction directory: %w", err)
	}
	stage := &StagedArchive{directory: directory, target: target}
	if err := ExtractArchive(ctx, archive, directory, archiveRoot, options, limits); err != nil {
		return nil, errors.Join(err, stage.Close())
	}
	return stage, nil
}

func (stage *StagedArchive) Publish() error {
	return PublishArchive(filepath.Join(stage.directory, ArchivePayloadRoot), stage.target)
}

func (stage *StagedArchive) Close() error {
	// Only this private, validated staging tree is repaired for removal. Never
	// change permissions on the destination or a recovery backup.
	_ = filepath.WalkDir(stage.directory, func(name string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			return os.Chmod(name, 0o700)
		}
		return err
	})
	return os.RemoveAll(stage.directory)
}
