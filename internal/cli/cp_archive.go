package cli

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

const copyArchivePayloadRoot = "payload"

type copyArchiveLimits struct {
	maxEntries         int
	maxFileBytes       int64
	maxTotalBytes      int64
	maxCompressedBytes int64
}

var defaultCopyArchiveLimits = copyArchiveLimits{
	maxEntries:         1_000_000,
	maxFileBytes:       64 << 30,
	maxTotalBytes:      256 << 30,
	maxCompressedBytes: 256 << 30,
}

var copyArchiveTargetMutexes sync.Map

type copyArchiveSource struct {
	base         string
	contentsOnly bool
}

func copyOverResolvedSSHArchive(ctx context.Context, session *sshTransportSession, target SSHTarget, src, dst string, followLink bool, stdout, stderr anyWriter) error {
	srcRemote, srcPath := sandboxCopyPath(src)
	_, dstPath := sandboxCopyPath(dst)
	if strings.TrimSpace(srcPath) == "" || strings.TrimSpace(dstPath) == "" {
		return exit(2, "copy source and destination paths must not be empty")
	}
	if srcRemote {
		return downloadResolvedSSHArchive(ctx, session, target, srcPath, dstPath, stdout, stderr, defaultCopyArchiveLimits)
	}
	return uploadResolvedSSHArchive(ctx, session, target, srcPath, dstPath, followLink, stdout, stderr, defaultCopyArchiveLimits)
}

func uploadResolvedSSHArchive(ctx context.Context, session *sshTransportSession, target SSHTarget, localPath, remotePath string, followLink bool, stdout, stderr anyWriter, limits copyArchiveLimits) error {
	source, archive, err := createCopyArchive(ctx, localPath, followLink, limits)
	if err != nil {
		return err
	}
	defer func() {
		name := archive.Name()
		_ = archive.Close()
		_ = os.Remove(name)
	}()
	remote, err := remoteCopyArchiveUploadCommand(remotePath, source.base, source.contentsOnly)
	if err != nil {
		return err
	}
	if err := runResolvedSSHArchiveCommand(ctx, session, target, remote, archive, stdout, stderr); err != nil {
		return fmt.Errorf("copy archive over resolved SSH transport: %w", err)
	}
	return nil
}

func downloadResolvedSSHArchive(ctx context.Context, session *sshTransportSession, target SSHTarget, remotePath, localPath string, stdout, stderr anyWriter, limits copyArchiveLimits) error {
	archiveRoot, contentsOnly, err := remoteCopyArchiveRoot(remotePath)
	if err != nil {
		return err
	}
	remote, err := remoteCopyArchiveDownloadCommand(remotePath)
	if err != nil {
		return err
	}
	archive, err := os.CreateTemp("", "crabbox-cp-download-*.tgz")
	if err != nil {
		return fmt.Errorf("create copy archive temp file: %w", err)
	}
	defer func() {
		name := archive.Name()
		_ = archive.Close()
		_ = os.Remove(name)
	}()
	bounded := &copyArchiveBoundedWriter{writer: archive, remaining: limits.maxCompressedBytes, limit: limits.maxCompressedBytes}
	if err := runResolvedSSHArchiveCommand(ctx, session, target, remote, nil, bounded, stderr); err != nil {
		if bounded.limitErr != nil {
			return bounded.limitErr
		}
		return fmt.Errorf("copy archive over resolved SSH transport: %w", err)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind copy archive: %w", err)
	}
	targetPath, err := localCopyArchiveTarget(localPath, archiveRoot, contentsOnly)
	if err != nil {
		return err
	}
	parent := filepath.Dir(targetPath)
	if info, err := os.Stat(parent); err != nil {
		return exit(2, "copy destination parent is unavailable: %v", err)
	} else if !info.IsDir() {
		return exit(2, "copy destination parent is not a directory: %s", parent)
	}
	stage, err := os.MkdirTemp(parent, ".crabbox-cp-*")
	if err != nil {
		return fmt.Errorf("create copy extraction directory: %w", err)
	}
	defer os.RemoveAll(stage)
	if err := extractValidatedCopyArchive(ctx, archive, stage, archiveRoot, limits); err != nil {
		return err
	}
	payload := filepath.Join(stage, copyArchivePayloadRoot)
	if err := syncCopyArchiveTree(payload); err != nil {
		return err
	}
	if err := publishCopyArchivePayload(payload, targetPath); err != nil {
		return err
	}
	return nil
}

type copyArchiveBoundedWriter struct {
	writer    io.Writer
	remaining int64
	limit     int64
	limitErr  error
}

func (w *copyArchiveBoundedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		w.limitErr = exit(2, "copy archive exceeds compressed size limit (%d bytes)", w.limit)
		return 0, w.limitErr
	}
	n, err := w.writer.Write(data)
	w.remaining -= int64(n)
	return n, err
}

func runResolvedSSHArchiveCommand(ctx context.Context, session *sshTransportSession, target SSHTarget, remote string, stdin io.Reader, stdout io.Writer, stderr anyWriter) error {
	args := append(session.commandPrefix(), "-T", "--", session.host(), wrapRemoteForTarget(target, remote))
	handle := pondMeshExecCommand(ctx, target.ChildEnvDenylist, "ssh", args...)
	execHandle, ok := handle.(*pondMeshExecHandle)
	if !ok {
		return errors.New("resolved SSH archive transport does not expose process streams")
	}
	stderrTail := newSynchronizedTailBuffer(failureTailLines)
	execHandle.cmd.Stdin = stdin
	execHandle.cmd.Stdout = stdout
	execHandle.cmd.Stderr = stderrTail
	if err := handle.Start(); err != nil {
		return fmt.Errorf("start archive copy over resolved SSH transport: %w", err)
	}
	waitErr := handle.Wait()
	writeSSHTransportDiagnostic(stderr, target, stderrTail.String())
	if waitErr != nil {
		if cause := context.Cause(ctx); cause != nil && handle.WasTerminatedByOurCancel() {
			return cause
		}
		return waitErr
	}
	return nil
}

func createCopyArchive(ctx context.Context, sourcePath string, followLink bool, limits copyArchiveLimits) (_ copyArchiveSource, _ *os.File, err error) {
	trimmedSource := strings.TrimRight(sourcePath, "/")
	if trimmedSource != "" {
		lastComponent := trimmedSource
		if index := strings.LastIndexByte(trimmedSource, '/'); index >= 0 {
			lastComponent = trimmedSource[index+1:]
		}
		if lastComponent == "." || lastComponent == ".." {
			return copyArchiveSource{}, nil, exit(2, "archive copy does not support local source paths ending in . or ..")
		}
	}
	abs, err := filepath.Abs(sourcePath)
	if err != nil {
		return copyArchiveSource{}, nil, exit(2, "resolve copy source: %v", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return copyArchiveSource{}, nil, exit(2, "read copy source: %v", err)
	}
	semanticInfo := info
	if followLink && info.Mode()&os.ModeSymlink != 0 {
		semanticInfo, err = os.Stat(abs)
		if err != nil {
			return copyArchiveSource{}, nil, exit(2, "follow copy source: %v", err)
		}
	}
	if hasTrailingPathSeparator(sourcePath) && !semanticInfo.IsDir() {
		return copyArchiveSource{}, nil, exit(2, "archive copy source with trailing slash is not a directory")
	}
	contentsOnly := hasTrailingPathSeparator(sourcePath) && semanticInfo.IsDir()
	source := copyArchiveSource{base: filepath.Base(abs), contentsOnly: contentsOnly}
	archive, err := os.CreateTemp("", "crabbox-cp-upload-*.tgz")
	if err != nil {
		return copyArchiveSource{}, nil, fmt.Errorf("create copy archive temp file: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			name := archive.Name()
			_ = archive.Close()
			_ = os.Remove(name)
		}
	}()
	gz := gzip.NewWriter(archive)
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
		return copyArchiveSource{}, nil, exit(2, "open copy source root: %v", err)
	}
	anchoredInfo, err := root.Lstat(rootRelative)
	if err != nil || !os.SameFile(info, anchoredInfo) {
		_ = root.Close()
		_ = tw.Close()
		_ = gz.Close()
		return copyArchiveSource{}, nil, exit(2, "copy source changed before it could be archived")
	}
	state := &copyArchiveCreateState{ctx: ctx, writer: tw, limits: limits, followLink: followLink}
	appendErr := state.append(root, rootRelative, copyArchivePayloadRoot)
	closeRootErr := root.Close()
	if appendErr != nil {
		_ = tw.Close()
		_ = gz.Close()
		return copyArchiveSource{}, nil, appendErr
	}
	if closeRootErr != nil {
		_ = tw.Close()
		_ = gz.Close()
		return copyArchiveSource{}, nil, fmt.Errorf("close copy source root: %w", closeRootErr)
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return copyArchiveSource{}, nil, fmt.Errorf("finish copy archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return copyArchiveSource{}, nil, fmt.Errorf("finish checksummed copy archive: %w", err)
	}
	if info, err := archive.Stat(); err != nil {
		return copyArchiveSource{}, nil, fmt.Errorf("stat copy archive: %w", err)
	} else if info.Size() > limits.maxCompressedBytes {
		return copyArchiveSource{}, nil, exit(2, "copy archive exceeds compressed size limit (%d bytes)", limits.maxCompressedBytes)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return copyArchiveSource{}, nil, fmt.Errorf("rewind copy archive: %w", err)
	}
	keep = true
	return source, archive, nil
}

type copyArchiveCreateState struct {
	ctx        context.Context
	writer     *tar.Writer
	limits     copyArchiveLimits
	followLink bool
	entries    int
	totalBytes int64
	activeDirs []os.FileInfo
}

func (s *copyArchiveCreateState) append(root *os.Root, name, archiveName string) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	info, err := root.Lstat(name)
	if err != nil {
		return exit(2, "read copy source %s: %v", archiveName, err)
	}
	linkName := ""
	if info.Mode()&os.ModeSymlink != 0 {
		if s.followLink {
			resolved, resolveErr := filepath.EvalSymlinks(filepath.Join(root.Name(), name))
			if resolveErr != nil {
				return exit(2, "follow copy source symlink %s: %v", archiveName, resolveErr)
			}
			resolvedRoot, openErr := os.OpenRoot(filepath.Dir(resolved))
			if openErr != nil {
				return exit(2, "open followed copy source symlink %s: %v", archiveName, openErr)
			}
			defer resolvedRoot.Close()
			return s.append(resolvedRoot, filepath.Base(resolved), archiveName)
		} else {
			return exit(2, "archive copy source contains a symlink; use -L to follow it: %s", archiveName)
		}
	}
	var opened *os.File
	if info.Mode()&os.ModeSymlink == 0 {
		opened, err = root.Open(name)
		if err != nil {
			return exit(2, "open copy source %s without following links: %v", archiveName, err)
		}
		openedInfo, statErr := opened.Stat()
		if statErr != nil {
			_ = opened.Close()
			return exit(2, "verify opened copy source %s: %v", archiveName, statErr)
		}
		if !os.SameFile(info, openedInfo) || info.Mode().Type() != openedInfo.Mode().Type() {
			_ = opened.Close()
			return exit(2, "copy source changed while it was being archived: %s", archiveName)
		}
		info = openedInfo
		defer opened.Close()
	}
	if !info.Mode().IsRegular() && !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return exit(2, "copy archive source contains unsupported special file: %s", archiveName)
	}
	s.entries++
	if s.entries > s.limits.maxEntries {
		return exit(2, "copy archive exceeds entry limit (%d)", s.limits.maxEntries)
	}
	if info.Mode().IsRegular() {
		if info.Size() > s.limits.maxFileBytes {
			return exit(2, "copy archive file exceeds size limit (%d bytes): %s", s.limits.maxFileBytes, archiveName)
		}
		s.totalBytes += info.Size()
		if s.totalBytes > s.limits.maxTotalBytes {
			return exit(2, "copy archive exceeds total size limit (%d bytes)", s.limits.maxTotalBytes)
		}
	}
	header, err := tar.FileInfoHeader(info, linkName)
	if err != nil {
		return exit(2, "create copy archive header %s: %v", archiveName, err)
	}
	header.Name = filepath.ToSlash(archiveName)
	header.Format = tar.FormatPAX
	if err := s.writer.WriteHeader(header); err != nil {
		return fmt.Errorf("write copy archive header %s: %w", archiveName, err)
	}
	if info.Mode().IsRegular() {
		copyErr := copySyncArchiveMember(s.ctx, s.writer, opened)
		if copyErr != nil {
			return fmt.Errorf("archive copy source %s: %w", archiveName, copyErr)
		}
	}
	if !info.IsDir() {
		return nil
	}
	for _, active := range s.activeDirs {
		if os.SameFile(active, info) {
			return exit(2, "copy source symlink cycle at %s", archiveName)
		}
	}
	s.activeDirs = append(s.activeDirs, info)
	defer func() { s.activeDirs = s.activeDirs[:len(s.activeDirs)-1] }()
	children, err := opened.ReadDir(-1)
	if err != nil {
		return exit(2, "read copy source directory %s: %v", archiveName, err)
	}
	subroot, err := root.OpenRoot(name)
	if err != nil {
		return exit(2, "open copy source directory %s: %v", archiveName, err)
	}
	defer subroot.Close()
	subrootInfo, err := subroot.Stat(".")
	if err != nil || !os.SameFile(info, subrootInfo) {
		return exit(2, "copy source changed while it was being archived: %s", archiveName)
	}
	for _, child := range children {
		if err := s.append(subroot, child.Name(), path.Join(archiveName, filepath.ToSlash(child.Name()))); err != nil {
			return err
		}
	}
	return nil
}

func hasTrailingPathSeparator(value string) bool {
	return strings.HasSuffix(value, "/")
}

func remoteCopyArchiveRoot(remotePath string) (string, bool, error) {
	if err := validateRemoteCopyArchivePath(remotePath); err != nil {
		return "", false, err
	}
	trimmed := strings.TrimRight(remotePath, "/")
	if trimmed == "" || trimmed == "~" {
		return "", false, exit(2, "archive copy of a remote filesystem root or bare home is unsupported; use a path below it")
	}
	root := path.Base(trimmed)
	lastComponent := trimmed
	if index := strings.LastIndexByte(trimmed, '/'); index >= 0 {
		lastComponent = trimmed[index+1:]
	}
	if lastComponent == "." || lastComponent == ".." {
		return "", false, exit(2, "archive copy does not support remote source paths ending in . or ..")
	}
	if root == "." || root == ".." || root == "/" {
		return "", false, exit(2, "archive copy requires a named remote source path")
	}
	return root, strings.HasSuffix(remotePath, "/"), nil
}

func validateRemoteCopyArchivePath(remotePath string) error {
	if strings.TrimSpace(remotePath) == "" {
		return exit(2, "remote copy paths must not be empty")
	}
	if strings.ContainsAny(remotePath, "\x00\r\n") {
		return exit(2, "remote copy paths must not contain control characters")
	}
	if strings.HasPrefix(remotePath, "~") && remotePath != "~" && !strings.HasPrefix(remotePath, "~/") {
		return exit(2, "archive copy does not support named-user ~ paths; use an absolute path")
	}
	return nil
}

func remoteCopyArchivePathResolver() string {
	return `resolve_path() {
  case "$1" in
    "~") printf '%s\n' "$HOME" ;;
    "~/"*) printf '%s/%s\n' "$HOME" "${1#\~/}" ;;
    /*) printf '%s\n' "$1" ;;
    *) printf '%s/%s\n' "$PWD" "$1" ;;
  esac
}`
}

func remoteCopyArchiveUploadCommand(remotePath, sourceBase string, contentsOnly bool) (string, error) {
	if err := validateRemoteCopyArchivePath(remotePath); err != nil {
		return "", err
	}
	if sourceBase == "" || sourceBase == "." || strings.ContainsAny(sourceBase, "/\x00\r\n") {
		return "", exit(2, "copy source has an unsupported base name")
	}
	var script strings.Builder
	script.WriteString("set -euo pipefail\n")
	script.WriteString("export COPYFILE_DISABLE=1\n")
	script.WriteString(remoteCopyArchivePathResolver())
	script.WriteByte('\n')
	script.WriteString("for tool in tar gzip mktemp mkdir mv dirname basename find chmod ln awk ps sync; do command -v \"$tool\" >/dev/null || { echo \"archive copy requires remote $tool\" >&2; exit 127; }; done\n")
	script.WriteString("dest=$(resolve_path ")
	script.WriteString(shellQuote(remotePath))
	script.WriteString(")\n")
	script.WriteString("if [ -L \"$dest\" ]; then echo 'archive copy refuses a symlink destination' >&2; exit 2; fi\n")
	if contentsOnly {
		script.WriteString("target=$dest\nwhile [ \"$target\" != / ] && [ \"${target%/}\" != \"$target\" ]; do target=${target%/}; done\n")
	} else {
		if strings.HasSuffix(remotePath, "/") {
			script.WriteString("while [ \"$dest\" != / ] && [ \"${dest%/}\" != \"$dest\" ]; do dest=${dest%/}; done\nif [ -L \"$dest\" ]; then echo 'archive copy refuses a symlink destination' >&2; exit 2; fi\nif [ -e \"$dest\" ] && [ ! -d \"$dest\" ]; then echo 'archive copy directory destination is not a directory' >&2; exit 2; fi\nif [ ! -e \"$dest\" ]; then mkdir \"$dest\"; sync; fi\n")
		}
		script.WriteString("if [ -d \"$dest\" ]; then target=$dest/")
		script.WriteString(shellQuote(sourceBase))
		script.WriteString("; else target=$dest; fi\n")
	}
	script.WriteString(`target_parent=$(dirname -- "$target")
if [ ! -d "$target_parent" ]; then echo 'archive copy destination parent is unavailable' >&2; exit 2; fi
if [ -L "$target" ]; then echo 'archive copy refuses a symlink target' >&2; exit 2; fi
backup="${target}.crabbox-cp-backup"
marker="${target}.crabbox-cp-transaction"
process_identity() {
  if [ -r "/proc/$1/stat" ]; then awk '{ print $22 }' "/proc/$1/stat"; else ps -o lstart= -p "$1" 2>/dev/null; fi
}
recover_transaction() {
  current_owner=${1:-}
  if [ -e "$marker" ] || [ -L "$marker" ]; then
    if [ ! -f "$marker" ] || [ -L "$marker" ] || [ ! -O "$marker" ] || [ -n "$(find "$marker" -prune ! -perm 600 -print)" ]; then
      echo 'archive copy found an unauthenticated transaction marker' >&2
      return 2
    fi
    owner=""
    owner_start=""
    { IFS= read -r owner; IFS= read -r owner_start; } < "$marker"
    case "$owner" in ''|*[!0-9]*) echo 'archive copy found an invalid transaction owner' >&2; return 2 ;; esac
    if [ -z "$owner_start" ]; then echo 'archive copy found invalid transaction owner identity' >&2; return 2; fi
    if [ "$owner" != "$current_owner" ] && kill -0 "$owner" 2>/dev/null; then
      live_start=$(process_identity "$owner" || true)
      if [ -z "$live_start" ] || [ "$live_start" = "$owner_start" ]; then
        echo 'another archive copy is active for this target' >&2
        return 2
      fi
    fi
    if [ -e "$backup" ] || [ -L "$backup" ]; then
      if [ -e "$target" ] || [ -L "$target" ]; then rm -rf -- "$backup"; else mv "$backup" "$target"; fi
      sync
    fi
    rm -f -- "$marker"
    sync
  elif [ -e "$backup" ] || [ -L "$backup" ]; then
    echo 'archive copy found a reserved backup without its transaction marker' >&2
    return 2
  fi
}
recover_transaction ""
stage=$(mktemp -d "$target_parent/.crabbox-cp.XXXXXX")
tar_errors=$(mktemp "${TMPDIR:-/tmp}/crabbox-cp-tar.XXXXXX")
marker_tmp=$(mktemp "$target_parent/.crabbox-cp-owner.XXXXXX")
cleanup() {
  recover_transaction "$$" || true
  rm -rf -- "$stage"
  rm -f -- "$tar_errors" "$marker_tmp"
}
trap cleanup EXIT INT TERM
set +e
gzip -dc | tar -C "$stage" -xpf - 2>"$tar_errors"
pipeline_status=$?
set -e
if [ "$pipeline_status" -ne 0 ] || [ -s "$tar_errors" ]; then
  cat "$tar_errors" >&2
  exit 2
fi
if ! { [ -e "$stage/payload" ] || [ -L "$stage/payload" ]; }; then echo 'archive copy payload is missing' >&2; exit 2; fi
sync
chmod 600 "$marker_tmp"
owner_start=$(process_identity "$$")
if [ -z "$owner_start" ]; then echo 'archive copy could not resolve its transaction owner identity' >&2; exit 2; fi
printf '%s\n%s\n' "$$" "$owner_start" > "$marker_tmp"
sync
if ! ln "$marker_tmp" "$marker"; then echo 'another archive copy is active for this target' >&2; exit 2; fi
sync
if [ -e "$target" ] || [ -L "$target" ]; then
  mv "$target" "$backup"
  sync
fi
if ! mv "$stage/payload" "$target"; then
  recover_transaction "$$" || true
  exit 2
fi
sync
if [ -e "$backup" ] || [ -L "$backup" ]; then
  rm -rf -- "$backup"
  sync
fi
rm -f -- "$marker"
sync
`)
	return "bash --noprofile --norc -c " + shellQuote(script.String()), nil
}

func remoteCopyArchiveDownloadCommand(remotePath string) (string, error) {
	_, directoryIntent, err := remoteCopyArchiveRoot(remotePath)
	if err != nil {
		return "", err
	}
	var script strings.Builder
	script.WriteString("set -euo pipefail\n")
	script.WriteString("export COPYFILE_DISABLE=1\n")
	script.WriteString(remoteCopyArchivePathResolver())
	script.WriteByte('\n')
	script.WriteString("for tool in tar gzip dirname basename find mktemp; do command -v \"$tool\" >/dev/null || { echo \"archive copy requires remote $tool\" >&2; exit 127; }; done\n")
	script.WriteString("src=$(resolve_path ")
	script.WriteString(shellQuote(strings.TrimRight(remotePath, "/")))
	script.WriteString(")\n")
	script.WriteString(`if [ -L "$src" ]; then echo 'archive copy refuses a remote symlink source' >&2; exit 2; fi
if [ ! -f "$src" ] && [ ! -d "$src" ]; then echo 'archive copy source must be a regular file or directory' >&2; exit 2; fi
`)
	if directoryIntent {
		script.WriteString("if [ ! -d \"$src\" ]; then echo 'archive copy source with trailing slash is not a directory' >&2; exit 2; fi\n")
	}
	script.WriteString(`
special=$(find "$src" \( -type l -o \( -type f -links +1 \) -o \( ! -type f ! -type d \) \) -print -quit)
if [ -n "$special" ]; then echo 'archive copy source contains a link or special file' >&2; exit 2; fi
parent=$(dirname -- "$src")
leaf=$(basename -- "$src")
tar_errors=$(mktemp "${TMPDIR:-/tmp}/crabbox-cp-tar.XXXXXX")
cleanup() { rm -f -- "$tar_errors"; }
trap cleanup EXIT INT TERM
set +e
tar -C "$parent" -cf - -- "$leaf" 2>"$tar_errors" | gzip -c
pipeline_status=$?
set -e
if [ "$pipeline_status" -ne 0 ] || [ -s "$tar_errors" ]; then
  cat "$tar_errors" >&2
  exit 2
fi
`)
	return "bash --noprofile --norc -c " + shellQuote(script.String()), nil
}

func localCopyArchiveTarget(localPath, sourceBase string, contentsOnly bool) (string, error) {
	directoryIntent := hasTrailingPathSeparator(localPath)
	abs, err := filepath.Abs(localPath)
	if err != nil {
		return "", exit(2, "resolve copy destination: %v", err)
	}
	info, statErr := os.Lstat(abs)
	if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", exit(2, "archive copy refuses a symlink destination: %s", localPath)
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		return "", exit(2, "read copy destination: %v", statErr)
	}
	if directoryIntent && statErr == nil && !info.IsDir() {
		return "", exit(2, "archive copy directory destination is not a directory: %s", localPath)
	}
	if contentsOnly {
		return abs, nil
	}
	if directoryIntent && os.IsNotExist(statErr) {
		if err := os.Mkdir(abs, 0o755); err != nil {
			return "", exit(2, "create copy destination directory: %v", err)
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

func extractValidatedCopyArchive(ctx context.Context, archive io.Reader, stage, archiveRoot string, limits copyArchiveLimits) error {
	buffered := bufio.NewReader(archive)
	gz, err := gzip.NewReader(buffered)
	if err != nil {
		return exit(2, "read checksummed copy archive: %v", err)
	}
	gz.Multistream(false)
	maxArchiveBytes := limits.maxTotalBytes + int64(limits.maxEntries)*4096 + 1<<20
	if maxArchiveBytes < limits.maxTotalBytes {
		return exit(2, "copy archive size limit overflow")
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
			return exit(2, "read copy archive: %v", nextErr)
		}
		entries++
		if entries > limits.maxEntries {
			return exit(2, "copy archive exceeds entry limit (%d)", limits.maxEntries)
		}
		clean, rel, err := validatedCopyArchiveEntryPath(header.Name, archiveRoot)
		if err != nil {
			return err
		}
		if seen[clean] {
			return exit(2, "copy archive contains duplicate entry: %s", clean)
		}
		seen[clean] = true
		destination := filepath.Join(stage, copyArchivePayloadRoot)
		if rel != "" {
			destination = filepath.Join(destination, filepath.FromSlash(rel))
		}
		// Defense in depth: validatedCopyArchiveEntryPath already rejects
		// traversal, but assert containment at the use site so the guarantee
		// is local and provable independent of the sanitizer.
		payloadRoot := filepath.Join(stage, copyArchivePayloadRoot)
		if destination != payloadRoot && !strings.HasPrefix(destination, payloadRoot+string(os.PathSeparator)) {
			return exit(2, "copy archive entry escapes the staging root: %q", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := directories.ensure(stage, archiveRoot, rel); err != nil {
				return err
			}
			directoryTimes = append(directoryTimes, copyArchiveDirectoryTime{path: destination, modTime: header.ModTime})
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > limits.maxFileBytes {
				return exit(2, "copy archive file exceeds size limit (%d bytes): %s", limits.maxFileBytes, clean)
			}
			totalBytes += header.Size
			if totalBytes > limits.maxTotalBytes {
				return exit(2, "copy archive exceeds total size limit (%d bytes)", limits.maxTotalBytes)
			}
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
				return exit(2, "copy archive entry conflicts with existing path: %s (%s)", clean, existing.Mode())
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("inspect copy archive destination: %w", err)
			}
			file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return fmt.Errorf("create copy archive file %s: %w", destination, err)
			}
			written, copyErr := io.CopyN(file, tr, header.Size)
			syncErr := file.Sync()
			closeErr := file.Close()
			if copyErr != nil || written != header.Size {
				return exit(2, "copy archive entry %s is truncated", clean)
			}
			if closeErr != nil {
				return fmt.Errorf("close copy archive file %s: %w", destination, closeErr)
			}
			if syncErr != nil {
				return fmt.Errorf("sync copy archive file %s: %w", destination, syncErr)
			}
			if err := applyCopyArchiveModTime(destination, header.ModTime); err != nil {
				return err
			}
		default:
			return exit(2, "copy archive contains unsupported link or special entry: %s", clean)
		}
	}
	trailerBuffer := make([]byte, 128*1024)
	for {
		n, readErr := limitedArchive.Read(trailerBuffer)
		for _, value := range trailerBuffer[:n] {
			if value != 0 {
				return exit(2, "copy archive contains data after its end marker")
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return exit(2, "verify copy archive checksum: %v", readErr)
		}
		if limitedArchive.N == 0 {
			return exit(2, "copy archive exceeds uncompressed size limit (%d bytes)", maxArchiveBytes)
		}
	}
	if limitedArchive.N == 0 {
		return exit(2, "copy archive exceeds uncompressed size limit (%d bytes)", maxArchiveBytes)
	}
	if err := gz.Close(); err != nil {
		return exit(2, "verify copy archive checksum: %v", err)
	}
	if _, err := buffered.Peek(1); err != io.EOF {
		if err == nil {
			return exit(2, "copy archive contains trailing compressed data")
		}
		return exit(2, "inspect copy archive trailer: %v", err)
	}
	payload := filepath.Join(stage, copyArchivePayloadRoot)
	if _, err := os.Lstat(payload); err != nil {
		return exit(2, "copy archive contained no payload")
	}
	for index := len(directoryTimes) - 1; index >= 0; index-- {
		if err := applyCopyArchiveModTime(directoryTimes[index].path, directoryTimes[index].modTime); err != nil {
			return err
		}
	}
	return nil
}

type copyArchiveDirectoryTime struct {
	path    string
	modTime time.Time
}

type copyArchiveDirectoryTracker struct {
	identities map[string]string
}

func (t *copyArchiveDirectoryTracker) ensure(stage, archiveRoot, rel string) error {
	if t.identities == nil {
		t.identities = make(map[string]string)
	}
	current := filepath.Join(stage, copyArchivePayloadRoot)
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
			return exit(2, "copy archive directory conflicts with existing path: %s", logical)
		}
		identity := copyArchiveDirectoryIdentity(current, info)
		if existing, ok := t.identities[identity]; ok {
			if existing != logical {
				return exit(2, "copy archive contains duplicate filesystem path aliases: %s and %s", existing, logical)
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
		return "", "", exit(2, "copy archive contains unsafe path: %q", name)
	}
	slashed := filepath.ToSlash(name)
	if strings.ContainsRune(slashed, '\x00') || path.IsAbs(slashed) {
		return "", "", exit(2, "copy archive contains unsafe path: %q", name)
	}
	clean := path.Clean(slashed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", exit(2, "copy archive contains unsafe path: %q", name)
	}
	if clean != archiveRoot && !strings.HasPrefix(clean, archiveRoot+"/") {
		return "", "", exit(2, "copy archive entry escapes its source root: %q", name)
	}
	rel := strings.TrimPrefix(clean, archiveRoot)
	rel = strings.TrimPrefix(rel, "/")
	if goos == "windows" && !validWindowsCopyArchivePath(rel) {
		return "", "", exit(2, "copy archive contains unsafe Windows path: %q", name)
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

func publishCopyArchivePayload(payload, target string) error {
	if _, err := os.Lstat(payload); err != nil {
		return fmt.Errorf("read staged copy payload: %w", err)
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
			return exit(2, "archive copy found reserved backup without transaction marker: %s", backup)
		}
		return nil
	}
	if !copyArchiveMarkerIsPrivate(markerInfo) {
		return exit(2, "archive copy found an unauthenticated transaction marker: %s", marker)
	}
	state, err := os.ReadFile(marker)
	if err != nil {
		return fmt.Errorf("read copy transaction state: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(state), "\n"), "\n")
	if len(lines) != 3 || lines[0] != "crabbox-cp-v1" || strings.TrimSpace(lines[2]) == "" {
		return exit(2, "archive copy found invalid transaction state: %s", marker)
	}
	owner, err := strconv.Atoi(lines[1])
	if err != nil || owner <= 0 {
		return exit(2, "archive copy found invalid transaction state: %s", marker)
	}
	if owner != currentOwner && copyArchiveProcessIsAlive(owner) {
		identity, ok := copyArchiveProcessIdentity(owner)
		if !ok || identity == lines[2] {
			return exit(2, "another archive copy is active for destination %s", target)
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
	fileLock := flock.New(lockPath, flock.SetPermissions(0o600))
	locked, err := fileLock.TryLock()
	if err != nil || !locked {
		mutex.Unlock()
		if err != nil {
			return nil, fmt.Errorf("lock copy destination: %w", err)
		}
		return nil, exit(2, "another archive copy is active for destination %s", target)
	}
	return func() {
		_ = fileLock.Unlock()
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

func syncCopyArchiveTree(root string) error {
	return filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return exit(2, "refusing to sync symlink in downloaded copy archive: %s", name)
		}
		file, err := os.Open(name)
		if err != nil {
			return fmt.Errorf("open staged copy archive path for sync: %w", err)
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if syncErr != nil && !(runtime.GOOS == "windows" && entry.IsDir()) {
			return fmt.Errorf("sync staged copy archive path %s: %w", name, syncErr)
		}
		return closeErr
	})
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
