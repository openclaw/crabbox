package runnerfs

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const ArchivePayloadRoot = "payload"

type ArchiveLimits struct {
	MaxEntries         int
	MaxFileBytes       int64
	MaxTotalBytes      int64
	MaxCompressedBytes int64
}

func DefaultArchiveLimits() ArchiveLimits {
	return ArchiveLimits{MaxEntries: 1_000_000, MaxFileBytes: 64 << 30, MaxTotalBytes: 256 << 30, MaxCompressedBytes: 256 << 30}
}

type ArchiveSource struct {
	Base         string `json:"base"`
	ContentsOnly bool   `json:"contentsOnly"`
}

type CreateOptions struct {
	FollowLinks     bool
	RejectHardLinks bool
}

type ExtractOptions struct {
	// Uploads preserve ordinary permission bits; downloads normalize them.
	PreservePermissions bool
}

func (limits ArchiveLimits) validate() error {
	if limits.MaxEntries < 1 || limits.MaxFileBytes < 1 || limits.MaxTotalBytes < 1 || limits.MaxCompressedBytes < 1 ||
		limits.MaxFileBytes == math.MaxInt64 || limits.MaxCompressedBytes == math.MaxInt64 ||
		int64(limits.MaxEntries) > (math.MaxInt64-(1<<20)-1)/4096 ||
		limits.MaxTotalBytes > math.MaxInt64-int64(limits.MaxEntries)*4096-(1<<20)-1 {
		return invalid("invalid copy archive limits")
	}
	return nil
}

type archiveBoundedWriter struct {
	writer    io.Writer
	remaining int64
	limit     int64
}

func (w *archiveBoundedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, invalid("copy archive exceeds compressed size limit (%d bytes)", w.limit)
	}
	n, err := w.writer.Write(data)
	w.remaining -= int64(n)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return n, err
}

var copyArchiveTargetMutexes sync.Map

type InvalidArchiveError struct{ Message string }

func (e InvalidArchiveError) Error() string { return e.Message }
func invalid(format string, args ...any) error {
	return InvalidArchiveError{fmt.Sprintf(format, args...)}
}

func CreateArchive(ctx context.Context, sourcePath string, options CreateOptions, limits ArchiveLimits) (_ ArchiveSource, _ *os.File, err error) {
	if err := limits.validate(); err != nil {
		return ArchiveSource{}, nil, err
	}
	trimmedSource := strings.TrimRight(filepath.ToSlash(sourcePath), "/")
	if trimmedSource != "" {
		lastComponent := trimmedSource
		if index := strings.LastIndexByte(trimmedSource, '/'); index >= 0 {
			lastComponent = trimmedSource[index+1:]
		}
		if lastComponent == "." || lastComponent == ".." {
			return ArchiveSource{}, nil, invalid("archive copy does not support local source paths ending in . or ..")
		}
	}
	abs, err := resolvePathParent(sourcePath)
	if err != nil {
		return ArchiveSource{}, nil, invalid("resolve copy source: %v", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return ArchiveSource{}, nil, invalid("read copy source: %v", err)
	}
	semanticInfo := info
	if options.FollowLinks && info.Mode()&os.ModeSymlink != 0 {
		semanticInfo, err = os.Stat(abs)
		if err != nil {
			return ArchiveSource{}, nil, invalid("follow copy source: %v", err)
		}
	}
	if hasTrailingPathSeparator(sourcePath) && !semanticInfo.IsDir() {
		return ArchiveSource{}, nil, invalid("archive copy source with trailing slash is not a directory")
	}
	contentsOnly := hasTrailingPathSeparator(sourcePath) && semanticInfo.IsDir()
	source := ArchiveSource{Base: filepath.Base(abs), ContentsOnly: contentsOnly}
	archive, err := os.CreateTemp("", "crabbox-cp-upload-*.tgz")
	if err != nil {
		return ArchiveSource{}, nil, fmt.Errorf("create copy archive temp file: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			name := archive.Name()
			_ = archive.Close()
			_ = os.Remove(name)
		}
	}()
	outputIdentity, err := archive.Stat()
	if err != nil {
		return ArchiveSource{}, nil, fmt.Errorf("stat copy archive output: %w", err)
	}
	gz := gzip.NewWriter(&archiveBoundedWriter{writer: archive, remaining: limits.MaxCompressedBytes, limit: limits.MaxCompressedBytes})
	tw := tar.NewWriter(gz)
	rootPath := filepath.Dir(abs)
	rootRelative := filepath.Base(abs)
	if info.IsDir() {
		rootPath = abs
		rootRelative = "."
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		_ = tw.Close()
		_ = gz.Close()
		return ArchiveSource{}, nil, invalid("open copy source root: %v", err)
	}
	anchoredInfo, err := root.Lstat(rootRelative)
	if err != nil || !os.SameFile(info, anchoredInfo) {
		_ = root.Close()
		_ = tw.Close()
		_ = gz.Close()
		return ArchiveSource{}, nil, invalid("copy source changed before it could be archived")
	}
	state := &copyArchiveCreateState{ctx: ctx, writer: tw, limits: limits, options: options, outputIdentity: outputIdentity}
	appendErr := state.append(root, rootRelative, ArchivePayloadRoot)
	closeRootErr := root.Close()
	if appendErr != nil {
		_ = tw.Close()
		_ = gz.Close()
		return ArchiveSource{}, nil, appendErr
	}
	if closeRootErr != nil {
		_ = tw.Close()
		_ = gz.Close()
		return ArchiveSource{}, nil, fmt.Errorf("close copy source root: %w", closeRootErr)
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return ArchiveSource{}, nil, fmt.Errorf("finish copy archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return ArchiveSource{}, nil, fmt.Errorf("finish checksummed copy archive: %w", err)
	}
	if info, err := archive.Stat(); err != nil {
		return ArchiveSource{}, nil, fmt.Errorf("stat copy archive: %w", err)
	} else if info.Size() > limits.MaxCompressedBytes {
		return ArchiveSource{}, nil, invalid("copy archive exceeds compressed size limit (%d bytes)", limits.MaxCompressedBytes)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return ArchiveSource{}, nil, fmt.Errorf("rewind copy archive: %w", err)
	}
	keep = true
	return source, archive, nil
}

type copyArchiveCreateState struct {
	ctx            context.Context
	writer         *tar.Writer
	limits         ArchiveLimits
	options        CreateOptions
	entries        int
	totalBytes     int64
	activeDirs     []os.FileInfo
	outputIdentity os.FileInfo
}

func (s *copyArchiveCreateState) append(root *os.Root, name, archiveName string) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	info, err := root.Lstat(name)
	if err != nil {
		return invalid("read copy source %s: %v", archiveName, err)
	}
	// The temporary directory can itself be the source. Exclude only this
	// archive's native identity, including aliases reached while following links.
	if s.outputIdentity != nil && os.SameFile(info, s.outputIdentity) {
		return nil
	}
	linkName := ""
	if info.Mode()&os.ModeSymlink != 0 {
		if s.options.FollowLinks {
			resolved, resolveErr := filepath.EvalSymlinks(filepath.Join(root.Name(), name))
			if resolveErr != nil {
				return invalid("follow copy source symlink %s: %v", archiveName, resolveErr)
			}
			resolvedRoot, openErr := os.OpenRoot(filepath.Dir(resolved))
			if openErr != nil {
				return invalid("open followed copy source symlink %s: %v", archiveName, openErr)
			}
			defer resolvedRoot.Close()
			return s.append(resolvedRoot, filepath.Base(resolved), archiveName)
		} else {
			return invalid("archive copy source contains a symlink; use -L to follow it: %s", archiveName)
		}
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return invalid("copy archive source contains unsupported special file: %s", archiveName)
	}
	var opened *os.File
	if info.Mode()&os.ModeSymlink == 0 {
		opened, err = root.OpenFile(name, os.O_RDONLY|nonblockingOpen, 0)
		if err != nil {
			return invalid("open copy source %s without following links: %v", archiveName, err)
		}
		openedInfo, statErr := opened.Stat()
		if statErr != nil {
			_ = opened.Close()
			return invalid("verify opened copy source %s: %v", archiveName, statErr)
		}
		if !os.SameFile(info, openedInfo) || info.Mode().Type() != openedInfo.Mode().Type() {
			_ = opened.Close()
			return invalid("copy source changed while it was being archived: %s", archiveName)
		}
		info = openedInfo
		defer opened.Close()
	}
	if !info.Mode().IsRegular() && !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return invalid("copy archive source contains unsupported special file: %s", archiveName)
	}
	s.entries++
	if s.entries > s.limits.MaxEntries {
		return invalid("copy archive exceeds entry limit (%d)", s.limits.MaxEntries)
	}
	if info.Mode().IsRegular() {
		if s.options.RejectHardLinks && archiveHasHardLinks(info) {
			return invalid("copy archive source contains a hard link: %s", archiveName)
		}
		if info.Size() > s.limits.MaxFileBytes {
			return invalid("copy archive file exceeds size limit (%d bytes): %s", s.limits.MaxFileBytes, archiveName)
		}
		if info.Size() > s.limits.MaxTotalBytes-s.totalBytes {
			return invalid("copy archive exceeds total size limit (%d bytes)", s.limits.MaxTotalBytes)
		}
		s.totalBytes += info.Size()
	}
	header, err := tar.FileInfoHeader(info, linkName)
	if err != nil {
		return invalid("create copy archive header %s: %v", archiveName, err)
	}
	header.Name = filepath.ToSlash(archiveName)
	header.Format = tar.FormatPAX
	if err := s.writer.WriteHeader(header); err != nil {
		return fmt.Errorf("write copy archive header %s: %w", archiveName, err)
	}
	if info.Mode().IsRegular() {
		copyErr := Copy(s.ctx, s.writer, io.LimitReader(opened, info.Size()+1))
		if copyErr != nil {
			return fmt.Errorf("archive copy source %s: %w", archiveName, copyErr)
		}
		after, err := opened.Stat()
		if err != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
			return invalid("copy source changed while it was being archived: %s", archiveName)
		}
	}
	if !info.IsDir() {
		return nil
	}
	for _, active := range s.activeDirs {
		if os.SameFile(active, info) {
			return invalid("copy source symlink cycle at %s", archiveName)
		}
	}
	s.activeDirs = append(s.activeDirs, info)
	defer func() { s.activeDirs = s.activeDirs[:len(s.activeDirs)-1] }()
	subroot, err := root.OpenRoot(name)
	if err != nil {
		return invalid("open copy source directory %s: %v", archiveName, err)
	}
	defer subroot.Close()
	subrootInfo, err := subroot.Stat(".")
	if err != nil || !os.SameFile(info, subrootInfo) {
		return invalid("copy source changed while it was being archived: %s", archiveName)
	}
	return readDirectory(s.ctx, opened, func(child os.DirEntry) error {
		return s.append(subroot, child.Name(), path.Join(archiveName, filepath.ToSlash(child.Name())))
	})
}

func hasTrailingPathSeparator(value string) bool {
	return len(value) > 0 && os.IsPathSeparator(value[len(value)-1])
}

// DownloadSource derives publication semantics from the caller's POSIX path,
// never from metadata supplied by the remote filesystem.
func DownloadSource(name string) (ArchiveSource, error) {
	if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\x00\r\n") || strings.HasPrefix(name, "~") && name != "~" && !strings.HasPrefix(name, "~/") {
		return ArchiveSource{}, invalid("unsupported remote copy source path")
	}
	trimmed := strings.TrimRight(name, "/")
	last := trimmed[strings.LastIndexByte(trimmed, '/')+1:]
	if trimmed == "" || trimmed == "~" || last == "." || last == ".." {
		return ArchiveSource{}, invalid("archive copy requires a named source below the filesystem root or home")
	}
	return ArchiveSource{Base: path.Base(trimmed), ContentsOnly: strings.HasSuffix(name, "/")}, nil
}

func ArchiveTarget(localPath, sourceBase string, contentsOnly bool) (string, error) {
	if sourceBase == "" || sourceBase == "." || sourceBase == ".." || strings.ContainsAny(sourceBase, "/\x00\r\n") || runtime.GOOS == "windows" && strings.Contains(sourceBase, "\\") {
		return "", invalid("copy source has an unsupported base name")
	}
	directoryIntent := hasTrailingPathSeparator(localPath)
	abs, err := resolvePathParent(localPath)
	if err != nil {
		return "", invalid("resolve copy destination: %v", err)
	}
	info, statErr := os.Lstat(abs)
	if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", invalid("archive copy refuses a symlink destination: %s", localPath)
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		return "", invalid("read copy destination: %v", statErr)
	}
	if directoryIntent && statErr == nil && !info.IsDir() {
		return "", invalid("archive copy directory destination is not a directory: %s", localPath)
	}
	if contentsOnly {
		return abs, nil
	}
	if directoryIntent && os.IsNotExist(statErr) {
		if err := os.Mkdir(abs, 0o755); err != nil {
			return "", invalid("create copy destination directory: %v", err)
		}
		if err := syncCopyArchiveDirectory(filepath.Dir(abs)); err != nil {
			return "", err
		}
		return filepath.Join(abs, sourceBase), nil
	}
	if statErr == nil && info.IsDir() {
		return filepath.Join(abs, sourceBase), nil
	}
	return abs, nil
}

func ExtractArchive(ctx context.Context, archive io.Reader, stage, archiveRoot string, options ExtractOptions, limits ArchiveLimits) error {
	if err := limits.validate(); err != nil {
		return err
	}
	compressed := &io.LimitedReader{R: archive, N: limits.MaxCompressedBytes + 1}
	buffered := bufio.NewReader(compressed)
	gz, err := gzip.NewReader(buffered)
	if err != nil {
		return invalid("read checksummed copy archive: %v", err)
	}
	gz.Multistream(false)
	maxArchiveBytes := limits.MaxTotalBytes + int64(limits.MaxEntries)*4096 + 1<<20
	if maxArchiveBytes < limits.MaxTotalBytes {
		return invalid("copy archive size limit overflow")
	}
	limitedArchive := &io.LimitedReader{R: gz, N: maxArchiveBytes + 1}
	tr := tar.NewReader(limitedArchive)
	seen := make(map[string]bool)
	directories := copyArchiveDirectoryTracker{}
	var directoryTimes []copyArchiveDirectoryTime
	entries := 0
	var totalBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return invalid("read copy archive: %v", nextErr)
		}
		entries++
		if entries > limits.MaxEntries {
			return invalid("copy archive exceeds entry limit (%d)", limits.MaxEntries)
		}
		if !filepath.IsLocal(header.Name) {
			return invalid("copy archive contains unsafe path: %q", header.Name)
		}
		clean, rel, err := validatedCopyArchiveEntryPath(header.Name, archiveRoot)
		if err != nil {
			return err
		}
		if seen[clean] {
			return invalid("copy archive contains duplicate entry: %s", clean)
		}
		seen[clean] = true
		destination := filepath.Join(stage, ArchivePayloadRoot)
		if rel != "" {
			destination = filepath.Join(destination, filepath.FromSlash(rel))
		}
		// Defense in depth: validatedCopyArchiveEntryPath already rejects
		// traversal, but assert containment at the use site so the guarantee
		// is local and provable independent of the sanitizer.
		payloadRoot := filepath.Join(stage, ArchivePayloadRoot)
		if destination != payloadRoot && !strings.HasPrefix(destination, payloadRoot+string(os.PathSeparator)) {
			return invalid("copy archive entry escapes the staging root: %q", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := directories.ensure(stage, archiveRoot, rel); err != nil {
				return err
			}
			directoryTimes = append(directoryTimes, copyArchiveDirectoryTime{path: destination, modTime: header.ModTime, mode: os.FileMode(header.Mode) & 0o777})
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > limits.MaxFileBytes {
				return invalid("copy archive file exceeds size limit (%d bytes): %s", limits.MaxFileBytes, clean)
			}
			if header.Size > limits.MaxTotalBytes-totalBytes {
				return invalid("copy archive exceeds total size limit (%d bytes)", limits.MaxTotalBytes)
			}
			totalBytes += header.Size
			if rel != "" {
				parentRel := path.Dir(rel)
				if parentRel == "." {
					parentRel = ""
				}
				if err := directories.ensure(stage, archiveRoot, parentRel); err != nil {
					return err
				}
			}
			if existing, err := os.Lstat(destination); err == nil {
				return invalid("copy archive entry conflicts with existing path: %s (%s)", clean, existing.Mode())
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("inspect copy archive destination: %w", err)
			}
			file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return fmt.Errorf("create copy archive file %s: %w", destination, err)
			}
			copyErr := Copy(ctx, file, tr)
			if copyErr == nil && options.PreservePermissions {
				copyErr = file.Chmod(os.FileMode(header.Mode) & 0o777)
			}
			if copyErr == nil {
				copyErr = applyCopyArchiveModTime(destination, header.ModTime)
			}
			syncErr := file.Sync()
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("extract copy archive entry %s: %w", clean, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close copy archive file %s: %w", destination, closeErr)
			}
			if syncErr != nil {
				return fmt.Errorf("sync copy archive file %s: %w", destination, syncErr)
			}
		default:
			return invalid("copy archive contains unsupported link or special entry: %s", clean)
		}
	}
	trailerBuffer := make([]byte, 128*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := limitedArchive.Read(trailerBuffer)
		for _, value := range trailerBuffer[:n] {
			if value != 0 {
				return invalid("copy archive contains data after its end marker")
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return invalid("verify copy archive checksum: %v", readErr)
		}
		if limitedArchive.N == 0 {
			return invalid("copy archive exceeds uncompressed size limit (%d bytes)", maxArchiveBytes)
		}
	}
	if limitedArchive.N == 0 {
		return invalid("copy archive exceeds uncompressed size limit (%d bytes)", maxArchiveBytes)
	}
	if err := gz.Close(); err != nil {
		return invalid("verify copy archive checksum: %v", err)
	}
	if _, err := buffered.Peek(1); err != io.EOF {
		if err == nil {
			return invalid("copy archive contains trailing compressed data")
		}
		return invalid("inspect copy archive trailer: %v", err)
	}
	if compressed.N <= 0 {
		return invalid("copy archive exceeds compressed size limit (%d bytes)", limits.MaxCompressedBytes)
	}
	payload := filepath.Join(stage, ArchivePayloadRoot)
	if _, err := os.Lstat(payload); err != nil {
		return invalid("copy archive contained no payload")
	}
	if err := filepath.WalkDir(stage, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return syncCopyArchiveDirectory(name)
		}
		return nil
	}); err != nil {
		return err
	}
	for index := len(directoryTimes) - 1; index >= 0; index-- {
		if err := applyCopyArchiveModTime(directoryTimes[index].path, directoryTimes[index].modTime); err != nil {
			return err
		}
		if err := syncCopyArchiveDirectory(directoryTimes[index].path); err != nil {
			return err
		}
		if options.PreservePermissions {
			directory, err := os.Open(directoryTimes[index].path)
			if err != nil {
				return err
			}
			modeErr := directory.Chmod(directoryTimes[index].mode)
			syncErr := directory.Sync()
			closeErr := directory.Close()
			if err := errors.Join(modeErr, syncErr, closeErr); err != nil {
				return fmt.Errorf("apply copy directory permissions: %w", err)
			}
		}
	}
	return nil
}

type copyArchiveDirectoryTime struct {
	path    string
	modTime time.Time
	mode    os.FileMode
}

type copyArchiveDirectoryTracker struct {
	identities map[string]string
}

func (t *copyArchiveDirectoryTracker) ensure(stage, archiveRoot, rel string) error {
	if t.identities == nil {
		t.identities = make(map[string]string)
	}
	current := filepath.Join(stage, ArchivePayloadRoot)
	logical := archiveRoot
	components := []string(nil)
	if rel != "" {
		components = strings.Split(rel, "/")
	}
	for index := -1; index < len(components); index++ {
		if index >= 0 {
			current = filepath.Join(current, filepath.FromSlash(components[index]))
			logical = path.Join(logical, components[index])
		}
		if err := os.Mkdir(current, 0o755); err != nil && !os.IsExist(err) {
			return fmt.Errorf("create copy archive directory %s: %w", current, err)
		}
		info, err := os.Stat(current)
		if err != nil {
			return fmt.Errorf("inspect copy archive directory %s: %w", current, err)
		}
		if !info.IsDir() {
			return invalid("copy archive directory conflicts with existing path: %s", logical)
		}
		identity := copyArchiveDirectoryIdentity(current, info)
		if existing, ok := t.identities[identity]; ok {
			if existing != logical {
				return invalid("copy archive contains duplicate filesystem path aliases: %s and %s", existing, logical)
			}
		} else {
			t.identities[identity] = logical
		}
	}
	return nil
}

func applyCopyArchiveModTime(name string, modTime time.Time) error {
	if modTime.IsZero() {
		return nil
	}
	if err := os.Chtimes(name, modTime, modTime); err != nil {
		return fmt.Errorf("apply copy archive timestamp to %s: %w", name, err)
	}
	return nil
}

func validatedCopyArchiveEntryPath(name, archiveRoot string) (string, string, error) {
	return validatedCopyArchiveEntryPathForGOOS(runtime.GOOS, name, archiveRoot)
}

func validatedCopyArchiveEntryPathForGOOS(goos, name, archiveRoot string) (string, string, error) {
	if goos == "windows" && strings.Contains(name, `\`) {
		return "", "", invalid("copy archive contains unsafe path: %q", name)
	}
	slashed := filepath.ToSlash(name)
	if strings.ContainsRune(slashed, '\x00') || path.IsAbs(slashed) {
		return "", "", invalid("copy archive contains unsafe path: %q", name)
	}
	for _, component := range strings.Split(slashed, "/") {
		if component == ".." {
			return "", "", invalid("copy archive contains unsafe path traversal component: %q", name)
		}
	}
	clean := path.Clean(slashed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", invalid("copy archive contains unsafe path: %q", name)
	}
	if clean != archiveRoot && !strings.HasPrefix(clean, archiveRoot+"/") {
		return "", "", invalid("copy archive entry escapes its source root: %q", name)
	}
	rel := strings.TrimPrefix(clean, archiveRoot)
	rel = strings.TrimPrefix(rel, "/")
	if goos == "windows" && !validWindowsCopyArchivePath(rel) {
		return "", "", invalid("copy archive contains unsafe Windows path: %q", name)
	}
	return clean, rel, nil
}

func validWindowsCopyArchivePath(rel string) bool {
	for _, component := range strings.Split(rel, "/") {
		if component == "" {
			continue
		}
		if strings.Contains(component, ":") || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return false
		}
		base := component
		if index := strings.IndexByte(base, '.'); index >= 0 {
			base = base[:index]
		}
		base = strings.ToUpper(base)
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
			len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
			return false
		}
	}
	return true
}

func PublishArchive(payload, target string) error {
	if _, err := os.Lstat(payload); err != nil {
		return fmt.Errorf("read staged copy payload: %w", err)
	}
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return invalid("archive copy refuses a symlink target: %s", target)
	}
	releaseLock, err := acquireCopyArchiveTargetLock(target)
	if err != nil {
		return err
	}
	defer releaseLock()
	backup := target + ".crabbox-cp-backup"
	marker := target + ".crabbox-cp-transaction"
	if err := recoverCopyArchiveTransaction(target, backup, marker, 0); err != nil {
		return err
	}
	if err := writeCopyArchiveTransactionMarker(marker); err != nil {
		return fmt.Errorf("write copy transaction marker: %w", err)
	}
	if _, err := os.Lstat(target); err == nil {
		if err := os.Rename(target, backup); err != nil {
			_ = os.Remove(marker)
			return fmt.Errorf("stage existing copy destination: %w", err)
		}
		if err := syncCopyArchiveDirectory(filepath.Dir(target)); err != nil {
			recoverErr := recoverCopyArchiveTransaction(target, backup, marker, os.Getpid())
			return errors.Join(err, recoverErr)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect copy destination: %w", err)
	}
	if err := os.Rename(payload, target); err != nil {
		recoverErr := recoverCopyArchiveTransaction(target, backup, marker, os.Getpid())
		return errors.Join(fmt.Errorf("publish copy archive: %w", err), recoverErr)
	}
	if err := syncCopyArchiveDirectory(filepath.Dir(target)); err != nil {
		recoverErr := recoverCopyArchiveTransaction(target, backup, marker, os.Getpid())
		return errors.Join(err, recoverErr)
	}
	if _, err := os.Lstat(backup); err == nil {
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("remove replaced copy destination: %w", err)
		}
		if err := syncCopyArchiveDirectory(filepath.Dir(target)); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect replaced copy destination: %w", err)
	}
	if err := os.Remove(marker); err != nil {
		return fmt.Errorf("remove copy transaction marker: %w", err)
	}
	if err := syncCopyArchiveDirectory(filepath.Dir(target)); err != nil {
		return err
	}
	return nil
}

func recoverCopyArchiveTransaction(target, backup, marker string, currentOwner int) error {
	markerInfo, markerErr := os.Lstat(marker)
	if markerErr != nil && !os.IsNotExist(markerErr) {
		return fmt.Errorf("inspect copy transaction marker: %w", markerErr)
	}
	_, backupErr := os.Lstat(backup)
	backupExists := backupErr == nil
	if backupErr != nil && !os.IsNotExist(backupErr) {
		return fmt.Errorf("inspect copy transaction backup: %w", backupErr)
	}
	if os.IsNotExist(markerErr) {
		if backupExists {
			return invalid("archive copy found reserved backup without transaction marker: %s", backup)
		}
		return nil
	}
	if !copyArchiveMarkerIsPrivate(markerInfo) {
		return invalid("archive copy found an unauthenticated transaction marker: %s", marker)
	}
	file, err := os.OpenFile(marker, os.O_RDONLY|nonblockingOpen, 0)
	if err != nil {
		return fmt.Errorf("read copy transaction state: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(markerInfo, openedInfo) || !copyArchiveMarkerIsPrivate(openedInfo) {
		return invalid("copy transaction marker changed before it could be read: %s", marker)
	}
	state, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(state) > 4096 {
		return invalid("invalid copy transaction state: %s", marker)
	}
	lines := strings.Split(strings.TrimSuffix(string(state), "\n"), "\n")
	// The former remote shell wrote two lines. Keep that persisted ownership
	// contract readable; new records retain the local v1 format.
	if len(lines) == 3 && lines[0] == "crabbox-cp-v1" {
		lines = lines[1:]
	}
	if len(lines) != 2 || strings.TrimSpace(lines[1]) == "" {
		return invalid("archive copy found invalid transaction state: %s", marker)
	}
	owner, err := strconv.Atoi(lines[0])
	if err != nil || owner <= 0 {
		return invalid("archive copy found invalid transaction state: %s", marker)
	}
	if owner != currentOwner && copyArchiveProcessIsAlive(owner) {
		identity, ok := copyArchiveProcessIdentity(owner)
		if !ok || strings.TrimSpace(identity) == strings.TrimSpace(lines[1]) {
			return invalid("another archive copy is active for destination %s", target)
		}
	}
	if backupExists {
		if _, targetErr := os.Lstat(target); targetErr == nil {
			if err := os.RemoveAll(backup); err != nil {
				return fmt.Errorf("remove completed copy transaction backup: %w", err)
			}
		} else if os.IsNotExist(targetErr) {
			if err := os.Rename(backup, target); err != nil {
				return fmt.Errorf("restore interrupted copy transaction: %w", err)
			}
		} else {
			return fmt.Errorf("inspect interrupted copy destination: %w", targetErr)
		}
		if err := syncCopyArchiveDirectory(filepath.Dir(target)); err != nil {
			return err
		}
	}
	if err := os.Remove(marker); err != nil {
		return fmt.Errorf("remove recovered copy transaction marker: %w", err)
	}
	if err := syncCopyArchiveDirectory(filepath.Dir(target)); err != nil {
		return err
	}
	return nil
}

func acquireCopyArchiveTargetLock(target string) (func(), error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("resolve copy transaction lock directory: %w", err)
	}
	lockDir := filepath.Join(cache, "crabbox", "cp-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("create copy transaction lock directory: %w", err)
	}
	if err := os.Chmod(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure copy transaction lock directory: %w", err)
	}
	canonicalParent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return nil, fmt.Errorf("canonicalize copy destination parent: %w", err)
	}
	canonicalTarget := filepath.Join(canonicalParent, filepath.Base(target))
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		canonicalTarget = strings.ToLower(canonicalTarget)
	}
	digest := sha256.Sum256([]byte(canonicalTarget))
	lockPath := filepath.Join(lockDir, fmt.Sprintf("%x.lock", digest[:]))
	value, _ := copyArchiveTargetMutexes.LoadOrStore(lockPath, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	unlock, locked, err := lockArchiveFile(lockPath)
	if err != nil || !locked {
		mutex.Unlock()
		if err != nil {
			return nil, fmt.Errorf("lock copy destination: %w", err)
		}
		return nil, invalid("another archive copy is active for destination %s", target)
	}
	return func() {
		unlock()
		mutex.Unlock()
	}, nil
}

func writeCopyArchiveTransactionMarker(name string) error {
	file, err := os.CreateTemp(filepath.Dir(name), ".crabbox-cp-marker-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	identity, ok := copyArchiveProcessIdentity(os.Getpid())
	if !ok {
		_ = file.Close()
		return errors.New("resolve copy transaction owner identity")
	}
	if _, err := fmt.Fprintf(file, "crabbox-cp-v1\n%d\n%s\n", os.Getpid(), identity); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(temporary, name); err != nil {
		return err
	}
	if err := syncCopyArchiveDirectory(filepath.Dir(name)); err != nil {
		return err
	}
	return nil
}

func syncCopyArchiveDirectory(name string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(name)
	if err != nil {
		return fmt.Errorf("open copy transaction directory for sync: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}
