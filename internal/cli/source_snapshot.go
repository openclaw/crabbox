package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	sourceSnapshotCleanupMaxAttempts = 3
	sourceSnapshotCleanupRetryDelay  = 50 * time.Millisecond
)

// sourceSnapshot owns staging and its cleanup capability, not repository
// identity, file selection, or acceptance policy.
type sourceSnapshot struct {
	Root        string
	cleanupRoot *sourceSnapshotRoot
}

type sourceSnapshotParentIdentity struct {
	path string
	info os.FileInfo
}

type sourceSnapshotParent struct {
	source      sourceSnapshotParentIdentity
	destination string
	relative    string
	handle      *os.File
	finalMode   os.FileMode
	tempMode    os.FileMode
	finalMTime  time.Time
	depth       int
}

type sourceSnapshotParents struct {
	sourceRoot sourceSnapshotParentIdentity
	byPath     map[string]*sourceSnapshotParent
	ordered    []*sourceSnapshotParent
}

type sourceSnapshotThaw func(*sourceSnapshotParents) error

type sourceSnapshotRoot struct {
	parent      *os.Root
	root        *os.Root
	name        string
	identity    os.FileInfo
	directories []*sourceSnapshotParent
}

func (snapshot *sourceSnapshot) cleanup() error {
	return snapshot.cleanupWith(func(string) error {
		if snapshot.cleanupRoot == nil {
			return fmt.Errorf("missing git overlay snapshot cleanup capability")
		}
		return snapshot.cleanupRoot.remove()
	}, time.Sleep)
}

func (snapshot *sourceSnapshot) cleanupWith(removeAll func(string) error, sleep func(time.Duration)) error {
	root := snapshot.Root
	if root == "" {
		return nil
	}
	var cleanupErr error
	for attempt := 1; attempt <= sourceSnapshotCleanupMaxAttempts; attempt++ {
		cleanupErr = removeAll(root)
		if cleanupErr == nil {
			snapshot.closeCleanupRoot()
			snapshot.Root = ""
			return nil
		}
		if attempt < sourceSnapshotCleanupMaxAttempts {
			sleep(sourceSnapshotCleanupRetryDelay)
		}
	}
	return fmt.Errorf("remove git overlay snapshot %q after %d attempts: %w", root, sourceSnapshotCleanupMaxAttempts, cleanupErr)
}

func (snapshot *sourceSnapshot) closeCleanupRoot() {
	if snapshot.cleanupRoot == nil {
		return
	}
	snapshot.cleanupRoot.close()
	snapshot.cleanupRoot = nil
}

func newSourceSnapshot() (sourceSnapshot, error) {
	root, err := os.MkdirTemp("", "crabbox-git-overlay-")
	if err != nil {
		return sourceSnapshot{}, err
	}
	parentPath := filepath.Dir(root)
	name := filepath.Base(root)
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		_ = os.Remove(root)
		return sourceSnapshot{}, err
	}
	cleanupRoot := &sourceSnapshotRoot{parent: parent, name: name}
	cleanupRoot.root, err = parent.OpenRoot(name)
	if err != nil {
		cleanupRoot.close()
		_ = os.Remove(root)
		return sourceSnapshot{}, err
	}
	cleanupRoot.identity, err = cleanupRoot.root.Stat(".")
	if err != nil {
		cleanupRoot.close()
		_ = os.Remove(root)
		return sourceSnapshot{}, err
	}
	if err := cleanupRoot.verifyIdentity(); err != nil {
		cleanupRoot.close()
		_ = os.Remove(root)
		return sourceSnapshot{}, err
	}
	return sourceSnapshot{Root: root, cleanupRoot: cleanupRoot}, nil
}

func (root *sourceSnapshotRoot) verifyIdentity() error {
	if root == nil || root.parent == nil || root.root == nil || root.identity == nil {
		return fmt.Errorf("incomplete git overlay snapshot cleanup capability")
	}
	opened, err := root.root.Stat(".")
	if err != nil {
		return fmt.Errorf("stat git overlay snapshot root handle: %w", err)
	}
	current, err := root.parent.Lstat(root.name)
	if err != nil {
		return fmt.Errorf("stat git overlay snapshot root path: %w", err)
	}
	if !opened.IsDir() ||
		!current.IsDir() ||
		current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(root.identity, opened) ||
		!os.SameFile(opened, current) {
		return fmt.Errorf("git overlay snapshot root identity changed")
	}
	return nil
}

func (root *sourceSnapshotRoot) remove() error {
	if err := root.verifyIdentity(); err != nil {
		return err
	}
	if err := root.thawDirectories(); err != nil {
		return err
	}
	if err := thawSourceSnapshotFiles(root.root); err != nil {
		return err
	}
	directory, err := root.root.Open(".")
	if err != nil {
		return fmt.Errorf("open git overlay snapshot root: %w", err)
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil {
		return fmt.Errorf("read git overlay snapshot root: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close git overlay snapshot root: %w", closeErr)
	}
	for _, entry := range entries {
		if err := root.root.RemoveAll(entry.Name()); err != nil {
			return fmt.Errorf("remove git overlay snapshot entry %q: %w", entry.Name(), err)
		}
	}
	if err := root.verifyIdentity(); err != nil {
		return err
	}
	if err := root.parent.Remove(root.name); err != nil {
		return fmt.Errorf("remove git overlay snapshot root: %w", err)
	}
	return nil
}

func (root *sourceSnapshotRoot) thawDirectories() error {
	for _, directory := range root.directories {
		if directory == nil || directory.handle == nil {
			continue
		}
		opened, err := directory.handle.Stat()
		if err != nil {
			return fmt.Errorf("stat git overlay snapshot cleanup handle %q: %w", directory.relative, err)
		}
		current, err := root.root.Lstat(filepath.ToSlash(directory.relative))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat git overlay snapshot cleanup path %q: %w", directory.relative, err)
		}
		if !opened.IsDir() ||
			!current.IsDir() ||
			current.Mode()&os.ModeSymlink != 0 ||
			!os.SameFile(opened, current) {
			return fmt.Errorf("git overlay snapshot cleanup path changed at %q", directory.relative)
		}
		if err := directory.handle.Chmod(directory.tempMode); err != nil {
			return fmt.Errorf("thaw git overlay snapshot directory %q: %w", directory.relative, err)
		}
	}
	return nil
}

func (root *sourceSnapshotRoot) close() {
	if root == nil {
		return
	}
	for _, directory := range root.directories {
		if directory != nil && directory.handle != nil {
			_ = directory.handle.Close()
			directory.handle = nil
		}
	}
	if root.root != nil {
		_ = root.root.Close()
		root.root = nil
	}
	if root.parent != nil {
		_ = root.parent.Close()
		root.parent = nil
	}
}

func (root *sourceSnapshotRoot) adopt(parents *sourceSnapshotParents) {
	if root == nil || parents == nil || len(parents.ordered) == 0 {
		return
	}
	root.directories = append(root.directories, parents.ordered...)
	parents.ordered = nil
	parents.byPath = nil
}
