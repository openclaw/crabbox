// Package runnerfs implements the bounded filesystem operations shared by local
// and remote runner endpoints. It has no provider, CLI, or network dependency.
package runnerfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrOutsideRoot = errors.New("file resolves outside the workspace")
	ErrNotRegular  = errors.New("not a regular file")
	ErrLimit       = errors.New("file exceeds the byte limit")
	ErrChanged     = errors.New("file changed while being read")
)

type Root struct {
	root *os.Root
	path string
}

type File struct {
	Path     string
	Data     []byte
	ModTime  time.Time
	identity os.FileInfo
}

func OpenRoot(name string) (*Root, error) {
	canonical, err := physicalAbsolutePath(name)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, err
	}
	return &Root{root: root, path: canonical}, nil
}

func (r *Root) Close() error { return r.root.Close() }

// Read resolves absolute in-workspace links before opening through the anchored
// root. Resolution alone never authorizes an ordinary path-based open: a later
// symlink or directory replacement must still pass os.Root's confinement.
func (r *Root) Read(name string, limit int64) (File, error) {
	if limit < 1 || limit >= 1<<62 {
		return File{}, errors.New("invalid file byte limit")
	}
	return r.readDistinct(context.Background(), name, limit, nil)
}

var errDuplicateReport = errors.New("report file already collected")

func (r *Root) readDistinct(ctx context.Context, name string, limit int64, seen []os.FileInfo) (File, error) {
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	file, err := r.openRegular(name)
	if err != nil {
		return File{}, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return File{}, err
	}
	for _, identity := range seen {
		if os.SameFile(identity, before) {
			return File{}, errDuplicateReport
		}
	}
	if limit <= 0 || before.Size() > limit {
		return File{}, ErrLimit
	}
	var buffer bytes.Buffer
	if err := Copy(ctx, &buffer, io.LimitReader(file, limit+1)); err != nil {
		return File{}, err
	}
	data := buffer.Bytes()
	if int64(len(data)) > limit {
		return File{}, ErrLimit
	}
	after, err := file.Stat()
	if err != nil {
		return File{}, err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(data)) != after.Size() {
		return File{}, ErrChanged
	}
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	return File{Path: name, Data: data, ModTime: after.ModTime(), identity: after}, nil
}

func (r *Root) openRegular(name string) (*os.File, error) {
	file, err := r.open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %q", ErrNotRegular, name)
	}
	return file, nil
}

func (r *Root) open(name string) (*os.File, error) {
	rel := name
	if filepath.IsAbs(name) {
		resolved, err := physicalAbsolutePath(name)
		if err != nil {
			return nil, err
		}
		rel, _ = filepath.Rel(r.path, resolved)
	}
	// Windows cleans parent components before os.Root opens them. Resolve those
	// paths physically first so relative and absolute report aliases agree.
	physicalParent := false
	if filepath.Separator == '\\' {
		for _, component := range strings.Split(filepath.ToSlash(rel), "/") {
			physicalParent = physicalParent || component == ".."
		}
	}
	if filepath.IsLocal(rel) && !physicalParent {
		file, err := r.root.OpenFile(rel, os.O_RDONLY|nonblockingOpen, 0)
		if err == nil {
			return file, nil
		}
	}
	candidate := name
	if !filepath.IsAbs(candidate) {
		candidate = strings.TrimRight(r.path, string(filepath.Separator)) + string(filepath.Separator) + candidate
	}
	resolved, err := physicalAbsolutePath(candidate)
	if err != nil {
		return nil, err
	}
	rel, err = filepath.Rel(r.path, resolved)
	if err != nil || !filepath.IsLocal(rel) || rel == "." {
		return nil, ErrOutsideRoot
	}
	return r.root.OpenFile(rel, os.O_RDONLY|nonblockingOpen, 0)
}
