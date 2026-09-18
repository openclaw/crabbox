package runnerfs

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path"
)

const directoryBatchSize = 128

type directoryReader interface {
	ReadDir(int) ([]os.DirEntry, error)
}

func readDirectory(ctx context.Context, directory directoryReader, visit func(os.DirEntry) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := directory.ReadDir(directoryBatchSize)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(entry); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Walk without ReadDir(-1): generated directories can be much larger than the
// small set of report candidates retained by the collector.
func (r *Root) walkDirectory(ctx context.Context, name string, visit func(string, fs.DirEntry) error) error {
	directory, err := r.root.OpenFile(name, os.O_RDONLY|nonblockingOpen, 0)
	if err != nil {
		return nil // Discovery, unlike an explicit read, ignores unreadable paths.
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil || !info.IsDir() {
		return nil
	}
	return readDirectory(ctx, directory, func(entry os.DirEntry) error {
		child := path.Join(name, entry.Name())
		if err := visit(child, entry); err != nil {
			if err == fs.SkipDir {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return r.walkDirectory(ctx, child, visit)
		}
		return nil
	})
}
