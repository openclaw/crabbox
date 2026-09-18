package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var errSourceSnapshotDrift = errors.New("local source changed during snapshot creation")

type sourceSnapshotHook func(phase string, attempt int, root string)

func copySourceSnapshot(ctx context.Context, sourceRoot, snapshotRoot string, paths []string) error {
	return copySourceSnapshotWithHook(ctx, sourceRoot, snapshotRoot, paths, 0, nil)
}

func copySourceSnapshotWithHook(ctx context.Context, sourceRoot, snapshotRoot string, paths []string, attempt int, hook sourceSnapshotHook) (result error) {
	return copySourceSnapshotContents(ctx, sourceRoot, snapshotRoot, paths, attempt, hook, nil)
}

func copySourceSnapshotOwned(ctx context.Context, sourceRoot string, snapshot *sourceSnapshot, paths []string, attempt int, hook sourceSnapshotHook) error {
	if snapshot == nil || snapshot.Root == "" || snapshot.cleanupRoot == nil {
		return fmt.Errorf("missing git overlay snapshot ownership")
	}
	return copySourceSnapshotContents(ctx, sourceRoot, snapshot.Root, paths, attempt, hook, snapshot.cleanupRoot)
}

func copySourceSnapshotContents(ctx context.Context, sourceRoot, snapshotRoot string, paths []string, attempt int, hook sourceSnapshotHook, owner *sourceSnapshotRoot) (result error) {
	return copySourceSnapshotContentsWithThaw(ctx, sourceRoot, snapshotRoot, paths, attempt, hook, owner, func(parents *sourceSnapshotParents) error {
		return parents.thaw()
	})
}

func copySourceSnapshotContentsWithThaw(ctx context.Context, sourceRoot, snapshotRoot string, paths []string, attempt int, hook sourceSnapshotHook, owner *sourceSnapshotRoot, thaw sourceSnapshotThaw) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	parents, err := newSourceSnapshotParents(sourceRoot)
	if err != nil {
		return err
	}
	defer func() {
		if result == nil {
			if owner != nil {
				owner.adopt(parents)
			}
			parents.close()
			return
		}
		thawErr := thaw(parents)
		if thawErr != nil && owner != nil {
			owner.adopt(parents)
		}
		parents.close()
		result = errors.Join(result, thawErr)
	}()
	for _, rel := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !safeRepoRel(rel) {
			return fmt.Errorf("unsafe overlay snapshot path %q", rel)
		}
		source := filepath.Join(sourceRoot, filepath.FromSlash(rel))
		destination := filepath.Join(snapshotRoot, filepath.FromSlash(rel))
		info, err := os.Lstat(source)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("%w: snapshot overlay path %q disappeared", errSourceSnapshotDrift, rel)
			}
			return fmt.Errorf("snapshot overlay path %q: %w", rel, err)
		}
		if hook != nil {
			hook("after_lstat", attempt, snapshotRoot)
		}
		identities, err := parents.ensure(snapshotRoot, filepath.Dir(rel), source, info)
		if err != nil {
			return fmt.Errorf("create overlay snapshot parent for %q: %w", rel, err)
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(source)
			if err != nil {
				return fmt.Errorf("read overlay snapshot symlink %q: %w", rel, classifySourceSnapshotError(source, info, err))
			}
			if err := os.Symlink(target, destination); err != nil {
				return fmt.Errorf("create overlay snapshot symlink %q: %w", rel, err)
			}
			finalMTime := normalizedSourceSnapshotFileTime(info.ModTime())
			if err := syncSourceSnapshotSymlinkTimes(destination, finalMTime); err != nil {
				return fmt.Errorf("restore overlay snapshot symlink mtime %q: %w", rel, err)
			}
			refreshed, err := os.Lstat(source)
			if err != nil {
				return fmt.Errorf("restat overlay snapshot symlink %q: %w", rel, classifySourceSnapshotError(source, info, err))
			}
			if !sameSourceSnapshotIdentity(info, refreshed) {
				return fmt.Errorf("%w: overlay snapshot symlink changed during copy at %q", errSourceSnapshotDrift, rel)
			}
			refreshedTarget, err := os.Readlink(source)
			if err != nil {
				return fmt.Errorf("reread overlay snapshot symlink %q: %w", rel, classifySourceSnapshotError(source, info, err))
			}
			if refreshedTarget != target {
				return fmt.Errorf("%w: overlay snapshot symlink target changed during copy at %q", errSourceSnapshotDrift, rel)
			}
			copied, err := os.Lstat(destination)
			if err != nil {
				return fmt.Errorf("stat overlay snapshot symlink destination %q: %w", rel, err)
			}
			if copied.Mode()&os.ModeSymlink == 0 || !copied.ModTime().Equal(finalMTime) {
				return fmt.Errorf("overlay snapshot symlink metadata changed at %q", rel)
			}
			copiedTarget, err := os.Readlink(destination)
			if err != nil {
				return fmt.Errorf("read overlay snapshot symlink destination %q: %w", rel, err)
			}
			if copiedTarget != target {
				return fmt.Errorf("overlay snapshot symlink target changed at %q", rel)
			}
		case info.Mode().IsRegular():
			if err := copySourceSnapshotFile(ctx, source, destination, info); err != nil {
				return fmt.Errorf("copy overlay snapshot file %q: %w", rel, err)
			}
		default:
			return fmt.Errorf("unsupported overlay snapshot file type at %q", rel)
		}
		if err := verifySourceSnapshotParents(identities); err != nil {
			return fmt.Errorf("verify overlay snapshot parent for %q: %w", rel, err)
		}
		if err := parents.verifyDestinations(false); err != nil {
			return fmt.Errorf("verify overlay snapshot destination parent for %q: %w", rel, err)
		}
	}
	if err := parents.restore(attempt, snapshotRoot, hook); err != nil {
		return err
	}
	return ctx.Err()
}

func newSourceSnapshotParents(sourceRoot string) (*sourceSnapshotParents, error) {
	rootInfo, err := os.Lstat(sourceRoot)
	if err != nil {
		return nil, err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("snapshot root is not a real directory")
	}
	return &sourceSnapshotParents{
		sourceRoot: sourceSnapshotParentIdentity{path: sourceRoot, info: rootInfo},
		byPath:     make(map[string]*sourceSnapshotParent),
	}, nil
}

func (parents *sourceSnapshotParents) ensure(snapshotRoot, parent, observedSource string, observedInfo os.FileInfo) ([]sourceSnapshotParentIdentity, error) {
	identities := []sourceSnapshotParentIdentity{parents.sourceRoot}
	if parent == "." || parent == "" {
		return identities, nil
	}
	source := parents.sourceRoot.path
	destination := snapshotRoot
	relative := ""
	for _, component := range strings.Split(filepath.ToSlash(parent), "/") {
		source = filepath.Join(source, filepath.FromSlash(component))
		destination = filepath.Join(destination, filepath.FromSlash(component))
		relative = filepath.Join(relative, filepath.FromSlash(component))
		info, err := os.Lstat(source)
		if err != nil {
			return nil, classifySourceSnapshotError(observedSource, observedInfo, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("snapshot parent is not a real directory")
		}
		if retained, ok := parents.byPath[destination]; ok {
			if !sameSourceSnapshotIdentity(retained.source.info, info) {
				return nil, fmt.Errorf("%w: snapshot parent changed during copy at %q", errSourceSnapshotDrift, source)
			}
			if err := verifySourceSnapshotDestination(retained, retained.tempMode, true, false); err != nil {
				return nil, err
			}
			identities = append(identities, retained.source)
			continue
		}
		identity := sourceSnapshotParentIdentity{path: source, info: info}
		finalMode := sourceSnapshotSupportedMode(info.Mode())
		tempMode := sourceSnapshotTemporaryDirectoryMode(finalMode)
		if err := os.Mkdir(destination, tempMode); err != nil {
			return nil, err
		}
		handle, err := openSourceSnapshotParent(destination)
		if err != nil {
			return nil, err
		}
		retained := &sourceSnapshotParent{
			source:      identity,
			destination: destination,
			relative:    relative,
			handle:      handle,
			finalMode:   finalMode,
			tempMode:    tempMode,
			finalMTime:  normalizedSourceSnapshotFileTime(info.ModTime()),
			depth:       len(identities),
		}
		parents.byPath[destination] = retained
		parents.ordered = append(parents.ordered, retained)
		if err := verifySourceSnapshotDestination(retained, 0, false, false); err != nil {
			return nil, err
		}
		if err := retained.handle.Chmod(tempMode); err != nil {
			return nil, err
		}
		if err := verifySourceSnapshotDestination(retained, tempMode, true, false); err != nil {
			return nil, err
		}
		identities = append(identities, identity)
	}
	if err := verifySourceSnapshotParents(identities); err != nil {
		return nil, err
	}
	return identities, nil
}

func (parents *sourceSnapshotParents) restore(attempt int, snapshotRoot string, hook sourceSnapshotHook) error {
	ordered := append([]*sourceSnapshotParent(nil), parents.ordered...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].depth != ordered[j].depth {
			return ordered[i].depth > ordered[j].depth
		}
		return ordered[i].destination < ordered[j].destination
	})
	for _, parent := range ordered {
		if hook != nil {
			hook("before_parent_mode_restore", attempt, snapshotRoot)
		}
		if err := verifySourceSnapshotParents([]sourceSnapshotParentIdentity{parents.sourceRoot, parent.source}); err != nil {
			return err
		}
		if err := verifySourceSnapshotDestination(parent, parent.tempMode, true, false); err != nil {
			return err
		}
		if err := syncSourceSnapshotFileTimes(parent.handle, parent.finalMTime); err != nil {
			return fmt.Errorf("restore overlay snapshot parent mtime at %q: %w", parent.destination, err)
		}
		if err := parent.handle.Chmod(parent.finalMode); err != nil {
			return fmt.Errorf("restore overlay snapshot parent mode at %q: %w", parent.destination, err)
		}
		if err := verifySourceSnapshotDestination(parent, parent.finalMode, true, true); err != nil {
			return err
		}
		if err := verifySourceSnapshotParents([]sourceSnapshotParentIdentity{parents.sourceRoot, parent.source}); err != nil {
			return err
		}
	}
	if err := verifySourceSnapshotParents(parents.sourceIdentities()); err != nil {
		return err
	}
	return parents.verifyDestinations(true)
}

func (parents *sourceSnapshotParents) sourceIdentities() []sourceSnapshotParentIdentity {
	identities := make([]sourceSnapshotParentIdentity, 0, len(parents.ordered)+1)
	identities = append(identities, parents.sourceRoot)
	for _, parent := range parents.ordered {
		identities = append(identities, parent.source)
	}
	return identities
}

func (parents *sourceSnapshotParents) verifyDestinations(final bool) error {
	for _, parent := range parents.ordered {
		mode := parent.tempMode
		if final {
			mode = parent.finalMode
		}
		if err := verifySourceSnapshotDestination(parent, mode, true, final); err != nil {
			return err
		}
	}
	return nil
}

func verifySourceSnapshotDestination(parent *sourceSnapshotParent, wantMode os.FileMode, checkMode, checkMTime bool) error {
	opened, err := parent.handle.Stat()
	if err != nil {
		return fmt.Errorf("stat overlay snapshot parent handle %q: %w", parent.destination, err)
	}
	current, err := os.Lstat(parent.destination)
	if err != nil {
		return fmt.Errorf("stat overlay snapshot parent path %q: %w", parent.destination, err)
	}
	if !opened.IsDir() ||
		!current.IsDir() ||
		current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(opened, current) {
		return fmt.Errorf("overlay snapshot parent path changed at %q", parent.destination)
	}
	if checkMode && (sourceSnapshotSupportedMode(opened.Mode()) != wantMode || sourceSnapshotSupportedMode(current.Mode()) != wantMode) {
		return fmt.Errorf("overlay snapshot parent mode at %q is %#o, want %#o", parent.destination, sourceSnapshotSupportedMode(current.Mode()), wantMode)
	}
	if checkMTime &&
		(!opened.ModTime().Equal(parent.finalMTime) || !current.ModTime().Equal(parent.finalMTime)) {
		return fmt.Errorf("overlay snapshot parent mtime at %q is %s, want %s", parent.destination, current.ModTime(), parent.finalMTime)
	}
	return nil
}

func sourceSnapshotSupportedMode(mode os.FileMode) os.FileMode {
	return mode & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}

func (parents *sourceSnapshotParents) thaw() error {
	var errs []error
	for _, parent := range parents.ordered {
		if parent.handle == nil {
			continue
		}
		if err := parent.handle.Chmod(parent.tempMode); err != nil {
			errs = append(errs, fmt.Errorf("thaw overlay snapshot parent %q: %w", parent.destination, err))
		}
	}
	return errors.Join(errs...)
}

func (parents *sourceSnapshotParents) close() {
	for _, parent := range parents.ordered {
		if parent.handle != nil {
			_ = parent.handle.Close()
			parent.handle = nil
		}
	}
}

func verifySourceSnapshotParents(identities []sourceSnapshotParentIdentity) error {
	for _, identity := range identities {
		refreshed, err := os.Lstat(identity.path)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("%w: snapshot parent changed during copy at %q: %v", errSourceSnapshotDrift, identity.path, err)
			}
			return fmt.Errorf("stat snapshot parent during copy at %q: %w", identity.path, err)
		}
		if !refreshed.IsDir() || refreshed.Mode()&os.ModeSymlink != 0 || !sameSourceSnapshotIdentity(identity.info, refreshed) {
			return fmt.Errorf("%w: snapshot parent changed during copy at %q", errSourceSnapshotDrift, identity.path)
		}
	}
	return nil
}

func copySourceSnapshotFile(ctx context.Context, source, destination string, info os.FileInfo) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, output.Close()) }()
	if _, err := copyObservedSourceFileBytes(ctx, output, source, info); err != nil {
		return err
	}
	if err := verifySourceSnapshotFileDestination(output, destination, 0, time.Time{}, false); err != nil {
		return err
	}
	finalMode := sourceSnapshotSupportedMode(info.Mode())
	finalMTime := normalizedSourceSnapshotFileTime(info.ModTime())
	if err := syncSourceSnapshotFileTimes(output, finalMTime); err != nil {
		return err
	}
	if err := output.Chmod(finalMode); err != nil {
		return err
	}
	return verifySourceSnapshotFileDestination(output, destination, finalMode, finalMTime, true)
}

// Both content hashing and staging consume the same observed, bounded file.
func copyObservedSourceFileBytes(ctx context.Context, dst io.Writer, source string, info os.FileInfo) (written int64, result error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	input, err := os.Open(source)
	if err != nil {
		return 0, classifySourceSnapshotError(source, info, err)
	}
	defer func() { result = errors.Join(result, input.Close(), ctx.Err()) }()
	openedInfo, err := input.Stat()
	if err != nil {
		return 0, err
	}
	if !openedInfo.Mode().IsRegular() || !sameSourceSnapshotIdentity(info, openedInfo) {
		return 0, fmt.Errorf("%w: source changed before reading", errSourceSnapshotDrift)
	}
	written, err = copySourceBytes(ctx, dst, input, openedInfo.Size())
	if err != nil {
		if errors.Is(err, errSourceCopyLimit) {
			return written, fmt.Errorf("%w: source grew beyond its accepted size", errSourceSnapshotDrift)
		}
		return written, err
	}
	postCopyInfo, err := input.Stat()
	if err != nil {
		return written, err
	}
	finalPathInfo, err := os.Lstat(source)
	if err != nil {
		return written, classifySourceSnapshotError(source, info, err)
	}
	if !sameSourceSnapshotIdentity(openedInfo, postCopyInfo) || !sameSourceSnapshotIdentity(postCopyInfo, finalPathInfo) || written != openedInfo.Size() {
		return written, fmt.Errorf("%w: source changed while reading", errSourceSnapshotDrift)
	}
	return written, nil
}

func verifySourceSnapshotFileDestination(file *os.File, path string, wantMode os.FileMode, wantMTime time.Time, checkMetadata bool) error {
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat overlay snapshot file handle %q: %w", path, err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat overlay snapshot file path %q: %w", path, err)
	}
	if !opened.Mode().IsRegular() ||
		!current.Mode().IsRegular() ||
		current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(opened, current) {
		return fmt.Errorf("overlay snapshot file path changed at %q", path)
	}
	if checkMetadata && (sourceSnapshotSupportedMode(opened.Mode()) != wantMode || sourceSnapshotSupportedMode(current.Mode()) != wantMode) {
		return fmt.Errorf("overlay snapshot file mode at %q is %#o, want %#o", path, sourceSnapshotSupportedMode(current.Mode()), wantMode)
	}
	if checkMetadata && (!opened.ModTime().Equal(wantMTime) || !current.ModTime().Equal(wantMTime)) {
		return fmt.Errorf("overlay snapshot file mtime at %q is %s, want %s", path, current.ModTime(), wantMTime)
	}
	return nil
}

func classifySourceSnapshotError(source string, observed os.FileInfo, operationErr error) error {
	refreshed, err := os.Lstat(source)
	if os.IsNotExist(err) || (err == nil && !sameSourceSnapshotIdentity(observed, refreshed)) {
		return fmt.Errorf("%w: snapshot source changed at %q", errSourceSnapshotDrift, source)
	}
	return operationErr
}

func sameSourceSnapshotIdentity(left, right os.FileInfo) bool {
	return left != nil &&
		right != nil &&
		os.SameFile(left, right) &&
		left.Mode() == right.Mode() &&
		left.Size() == right.Size() &&
		left.ModTime().Equal(right.ModTime())
}
