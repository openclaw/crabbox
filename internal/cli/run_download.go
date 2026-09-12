package cli

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	runDownloadMaxBytes              = maxPulledArtifactBytes
	runDownloadDiskReserveBytes      = int64(1024 * 1024 * 1024)
	failureCaptureDownloadMaxBytes   = int64(64 * 1024 * 1024)
	remoteDownloadHeaderPrefix       = "CRABBOX_DOWNLOAD_SIZE="
	remoteDownloadHeaderMaxBytes     = 128
	remoteDownloadDiagnosticMaxBytes = 4096
)

type runDownloadLimits struct {
	MaxBytes         int64
	DiskReserveBytes int64
}

var defaultRunDownloadLimits = runDownloadLimits{
	MaxBytes:         runDownloadMaxBytes,
	DiskReserveBytes: runDownloadDiskReserveBytes,
}

var failureCaptureDownloadLimits = runDownloadLimits{
	MaxBytes:         failureCaptureDownloadMaxBytes,
	DiskReserveBytes: runDownloadDiskReserveBytes,
}

type runDownloadSpec struct {
	Remote string
	Local  string
}

func parseRunDownloadSpec(value string) (runDownloadSpec, error) {
	remote, local, ok := strings.Cut(strings.TrimSpace(value), "=")
	remote = strings.TrimSpace(remote)
	local = strings.TrimSpace(local)
	if !ok || remote == "" || local == "" {
		return runDownloadSpec{}, exit(2, "--download expects remote=local")
	}
	return runDownloadSpec{Remote: remote, Local: local}, nil
}

func preflightRunLocalOutputs(captureStdout, captureStderr string, downloads []string) error {
	if captureStdout != "" && captureStderr != "" {
		same, err := sameLocalOutputPath(captureStdout, captureStderr)
		if err != nil {
			return err
		}
		if same {
			return exit(2, "capture stdout/stderr: paths must be different")
		}
	}
	parsedDownloads := make([]runDownloadSpec, 0, len(downloads))
	for _, spec := range downloads {
		download, err := parseRunDownloadSpec(spec)
		if err != nil {
			return err
		}
		parsedDownloads = append(parsedDownloads, download)
	}
	for _, download := range parsedDownloads {
		if err := rejectCaptureDownloadCollision("capture stdout", captureStdout, download); err != nil {
			return err
		}
		if err := rejectCaptureDownloadCollision("capture stderr", captureStderr, download); err != nil {
			return err
		}
	}
	for i := range parsedDownloads {
		for j := i + 1; j < len(parsedDownloads); j++ {
			same, err := sameLocalOutputPath(parsedDownloads[i].Local, parsedDownloads[j].Local)
			if err != nil {
				return err
			}
			if same {
				return exit(2, "download %s/download %s: paths must be different", parsedDownloads[i].Remote, parsedDownloads[j].Remote)
			}
		}
	}
	for _, download := range parsedDownloads {
		if err := preflightLocalOutputPath("download "+download.Remote, download.Local, true, true); err != nil {
			return err
		}
	}
	if captureStdout != "" {
		if err := preflightLocalOutputPath("capture stdout", captureStdout, false, true); err != nil {
			return err
		}
	}
	if captureStderr != "" {
		if err := preflightLocalOutputPath("capture stderr", captureStderr, false, true); err != nil {
			return err
		}
	}
	return nil
}

func rejectCaptureDownloadCollision(label, capturePath string, download runDownloadSpec) error {
	if capturePath == "" {
		return nil
	}
	same, err := sameLocalOutputPath(capturePath, download.Local)
	if err != nil {
		return err
	}
	if same {
		return exit(2, "%s/download %s: paths must be different", label, download.Remote)
	}
	return nil
}

func preflightProofOutputPath(proofPath, captureStdout, captureStderr string, downloads []string) error {
	return preflightRunOutputCollisions("emit proof", proofPath, captureStdout, captureStderr, downloads)
}

func preflightRunOutputCollisions(label, path, captureStdout, captureStderr string, downloads []string) error {
	if path == "" {
		return nil
	}
	if captureStdout != "" {
		same, err := sameLocalOutputPath(path, captureStdout)
		if err != nil {
			return err
		}
		if same {
			return exit(2, "%s/capture stdout: paths must be different", label)
		}
	}
	if captureStderr != "" {
		same, err := sameLocalOutputPath(path, captureStderr)
		if err != nil {
			return err
		}
		if same {
			return exit(2, "%s/capture stderr: paths must be different", label)
		}
	}
	for _, spec := range downloads {
		download, err := parseRunDownloadSpec(spec)
		if err != nil {
			return err
		}
		same, err := sameLocalOutputPath(path, download.Local)
		if err != nil {
			return err
		}
		if same {
			return exit(2, "%s/download %s: paths must be different", label, download.Remote)
		}
	}
	return nil
}

func sameLocalOutputPath(left, right string) (bool, error) {
	leftCanonical, err := canonicalLocalOutputPath(left)
	if err != nil {
		return false, exit(2, "local output path: %v", err)
	}
	rightCanonical, err := canonicalLocalOutputPath(right)
	if err != nil {
		return false, exit(2, "local output path: %v", err)
	}
	if leftCanonical == rightCanonical {
		return true, nil
	}
	leftInfo, leftErr := os.Stat(leftCanonical)
	rightInfo, rightErr := os.Stat(rightCanonical)
	if leftErr == nil && rightErr == nil {
		return os.SameFile(leftInfo, rightInfo), nil
	}
	for _, statErr := range []error{leftErr, rightErr} {
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return false, exit(2, "local output path: %v", statErr)
		}
	}
	if strings.EqualFold(leftCanonical, rightCanonical) {
		caseInsensitive, err := localPathCaseInsensitive(leftCanonical)
		if err != nil {
			return false, exit(2, "local output path case probe: %v", err)
		}
		if caseInsensitive {
			return true, nil
		}
	}
	return false, nil
}

func canonicalLocalOutputPath(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	return resolveLocalOutputSymlinks(abs, 0)
}

func resolveLocalOutputSymlinks(path string, depth int) (string, error) {
	// Resolve each existing link even when its target or a later component is not created yet.
	if depth > 64 {
		return "", fmt.Errorf("too many symlinks in %s", path)
	}
	volume := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path, volume)
	rest = strings.TrimLeft(rest, string(filepath.Separator))
	current := volume + string(filepath.Separator)
	if volume == "" {
		current = string(filepath.Separator)
	}
	parts := strings.FieldsFunc(rest, func(r rune) bool { return r == rune(filepath.Separator) })
	for i, part := range parts {
		candidate := filepath.Join(current, part)
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			return filepath.Join(append([]string{candidate}, parts[i+1:]...)...), nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			current = candidate
			continue
		}
		target, err := os.Readlink(candidate)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(candidate), target)
		}
		if i+1 < len(parts) {
			target = filepath.Join(append([]string{target}, parts[i+1:]...)...)
		}
		return resolveLocalOutputSymlinks(filepath.Clean(target), depth+1)
	}
	return current, nil
}

func localPathCaseInsensitive(path string) (bool, error) {
	dir := path
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				dir = filepath.Dir(dir)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false, err
		}
		dir = parent
	}
	// Probe the directory because supported platforms can host both case modes.
	probe, err := os.CreateTemp(dir, ".crabbox-case-Aa-*")
	if err != nil {
		return false, err
	}
	probePath := probe.Name()
	probeInfo, statErr := probe.Stat()
	closeErr := probe.Close()
	defer os.Remove(probePath)
	if statErr != nil {
		return false, statErr
	}
	if closeErr != nil {
		return false, closeErr
	}
	foldedPath := filepath.Join(dir, flipASCIICase(filepath.Base(probePath)))
	foldedInfo, err := os.Stat(foldedPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(probeInfo, foldedInfo), nil
}

func flipASCIICase(value string) string {
	flipped := []byte(value)
	for i, char := range flipped {
		switch {
		case char >= 'a' && char <= 'z':
			flipped[i] = char - ('a' - 'A')
		case char >= 'A' && char <= 'Z':
			flipped[i] = char + ('a' - 'A')
		}
	}
	return string(flipped)
}

func preflightLocalOutputPath(label, path string, allowMissingDirs, replaceExisting bool) error {
	dir := filepath.Dir(path)
	info, err := os.Stat(path)
	if replaceExisting {
		info, err = os.Lstat(path)
	}
	if err == nil {
		if info.IsDir() {
			return exit(2, "%s: %s is a directory", label, path)
		}
		if replaceExisting {
			if err := checkWritableDir(label, firstNonBlank(dir, ".")); err != nil {
				return err
			}
			return checkPrivateRunOutputReplaceable(label, path)
		}
		return checkWritableFile(label, path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return exit(2, "%s: %v", label, err)
	}
	if dir == "." || dir == "" {
		return checkWritableDir(label, ".")
	}
	if !allowMissingDirs {
		return checkWritableDir(label, dir)
	}
	existing := dir
	for {
		info, err := os.Stat(existing)
		if err == nil {
			if !info.IsDir() {
				return exit(2, "%s: %s is not a directory", label, existing)
			}
			return checkWritableDir(label, existing)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return exit(2, "%s: %v", label, err)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return exit(2, "%s: %v", label, err)
		}
		existing = parent
	}
}

func checkWritableFile(label, path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return exit(2, "%s: %v", label, err)
	}
	if err := file.Close(); err != nil {
		return exit(2, "%s close: %v", label, err)
	}
	return nil
}

func checkWritableDir(label, dir string) error {
	temp, err := os.CreateTemp(dir, ".crabbox-output-*")
	if err != nil {
		return exit(2, "%s: %v", label, err)
	}
	name := temp.Name()
	closeErr := temp.Close()
	removeErr := os.Remove(name)
	if closeErr != nil {
		return exit(2, "%s close: %v", label, closeErr)
	}
	if removeErr != nil {
		return exit(2, "%s cleanup: %v", label, removeErr)
	}
	return nil
}

func downloadRemoteFile(ctx context.Context, target SSHTarget, workdir, specValue string) (int, string, error) {
	return downloadRemoteFileWithLimits(ctx, target, workdir, specValue, defaultRunDownloadLimits)
}

func downloadRemoteFileWithLimits(ctx context.Context, target SSHTarget, workdir, specValue string, limits runDownloadLimits) (int, string, error) {
	spec, err := parseRunDownloadSpec(specValue)
	if err != nil {
		return 0, "", err
	}
	reader, writer := io.Pipe()
	type stageResult struct {
		stage *stagedRunDownload
		err   error
	}
	result := make(chan stageResult, 1)
	go func() {
		stage, stageErr := stageRunDownload(ctx, reader, spec.Local, limits, availableRunDownloadBytes)
		if stageErr != nil {
			_ = reader.CloseWithError(stageErr)
		}
		result <- stageResult{stage: stage, err: stageErr}
	}()
	diagnostic := newSynchronizedBuffer(remoteDownloadDiagnosticMaxBytes)
	code, streamErr := runSSHStreamResult(ctx, target, remoteDownloadBase64Command(target, workdir, spec.Remote), writer, &diagnostic)
	if streamErr != nil {
		_ = writer.CloseWithError(streamErr)
	} else {
		_ = writer.Close()
	}
	staged := <-result
	if staged.err != nil {
		var localErr runDownloadLocalError
		if errors.As(staged.err, &localErr) {
			return 0, spec.Local, exit(2, "download %s: write %s: %v", spec.Remote, spec.Local, localErr)
		}
		return 0, spec.Local, exit(7, "download %s: %v", spec.Remote, staged.err)
	}
	if streamErr != nil || code != 0 {
		staged.stage.remove()
		remoteErr := firstNonNil(streamErr, fmt.Errorf("remote command exited %d", code))
		if detail := strings.TrimSpace(diagnostic.String()); detail != "" {
			return 0, spec.Local, exit(7, "download %s: %v: %s", spec.Remote, remoteErr, detail)
		}
		return 0, spec.Local, exit(7, "download %s: %v", spec.Remote, remoteErr)
	}
	if err := staged.stage.publish(); err != nil {
		return 0, spec.Local, exit(2, "download %s: write %s: %v", spec.Remote, spec.Local, err)
	}
	return int(staged.stage.bytes), spec.Local, nil
}

func writeRunDownloadFile(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := createPrivateRunOutputDir(dir); err != nil {
			return err
		}
	}
	return writePrivateRunOutputFile(path, data)
}

type runDownloadLocalError struct {
	error
}

type stagedRunDownload struct {
	tempPath string
	path     string
	bytes    int64
}

func (download *stagedRunDownload) remove() {
	if download != nil && download.tempPath != "" {
		_ = os.Remove(download.tempPath)
		download.tempPath = ""
	}
}

func (download *stagedRunDownload) publish() error {
	if download == nil || download.tempPath == "" {
		return fmt.Errorf("download temporary file is unavailable")
	}
	tempPath := download.tempPath
	download.tempPath = ""
	if err := replacePrivateRunOutputTemp(tempPath, download.path); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	return nil
}

func stageRunDownload(ctx context.Context, source io.Reader, path string, limits runDownloadLimits, available func(string) (int64, error)) (_ *stagedRunDownload, resultErr error) {
	if limits.MaxBytes < 0 || limits.DiskReserveBytes < 0 {
		return nil, fmt.Errorf("invalid download limits")
	}
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := createPrivateRunOutputDir(dir); err != nil {
			return nil, runDownloadLocalError{err}
		}
	}
	file, tempPath, err := createPrivateRunOutputTemp(path)
	if err != nil {
		return nil, runDownloadLocalError{err}
	}
	defer func() {
		if resultErr != nil {
			_ = file.Close()
			_ = os.Remove(tempPath)
		}
	}()
	buffered := bufio.NewReaderSize(&contextRunDownloadReader{ctx: ctx, reader: source}, remoteDownloadHeaderMaxBytes)
	advertised, err := readRemoteDownloadSize(buffered)
	if err != nil {
		return nil, err
	}
	if advertised > limits.MaxBytes {
		return nil, fmt.Errorf("advertised size %d exceeds limit %d", advertised, limits.MaxBytes)
	}
	if err := checkRunDownloadDiskReserve(dir, advertised, limits.DiskReserveBytes, available); err != nil {
		return nil, runDownloadLocalError{err}
	}
	destination := &runDownloadReserveWriter{
		writer:    file,
		path:      dir,
		reserve:   limits.DiskReserveBytes,
		available: available,
	}
	written, exceeded, err := copyArtifactResponse(destination, base64.NewDecoder(base64.StdEncoding, buffered), advertised)
	if err != nil {
		return nil, fmt.Errorf("decode base64 stream: %w", err)
	}
	if exceeded {
		return nil, fmt.Errorf("decoded response exceeds advertised size %d", advertised)
	}
	if written != advertised {
		return nil, fmt.Errorf("decoded size %d does not match advertised size %d", written, advertised)
	}
	if err := file.Close(); err != nil {
		return nil, runDownloadLocalError{err}
	}
	return &stagedRunDownload{tempPath: tempPath, path: path, bytes: written}, nil
}

func readRemoteDownloadSize(reader *bufio.Reader) (int64, error) {
	line, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > remoteDownloadHeaderMaxBytes {
		return 0, fmt.Errorf("size header exceeds %d bytes", remoteDownloadHeaderMaxBytes)
	}
	if err != nil {
		return 0, fmt.Errorf("read size header: %w", err)
	}
	value, ok := strings.CutPrefix(strings.TrimSpace(string(line)), remoteDownloadHeaderPrefix)
	value = strings.TrimSpace(value)
	if !ok || value == "" {
		return 0, fmt.Errorf("invalid size header")
	}
	size, err := strconv.ParseInt(value, 10, 64)
	if err != nil || size < 0 || strconv.FormatInt(size, 10) != value {
		return 0, fmt.Errorf("invalid advertised size %q", value)
	}
	return size, nil
}

func checkRunDownloadDiskReserve(path string, incoming, reserve int64, available func(string) (int64, error)) error {
	free, err := available(firstNonBlank(path, "."))
	if err != nil {
		return fmt.Errorf("check available disk: %w", err)
	}
	if free < reserve || incoming > free-reserve {
		return fmt.Errorf("insufficient disk: available=%d incoming=%d reserve=%d", free, incoming, reserve)
	}
	return nil
}

type runDownloadReserveWriter struct {
	writer    io.Writer
	path      string
	reserve   int64
	available func(string) (int64, error)
}

func (writer *runDownloadReserveWriter) Write(data []byte) (int, error) {
	if err := checkRunDownloadDiskReserve(writer.path, int64(len(data)), writer.reserve, writer.available); err != nil {
		return 0, runDownloadLocalError{err}
	}
	written, err := writer.writer.Write(data)
	if err != nil {
		return written, runDownloadLocalError{err}
	}
	return written, nil
}

type contextRunDownloadReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextRunDownloadReader) Read(data []byte) (int, error) {
	if err := context.Cause(reader.ctx); err != nil {
		return 0, err
	}
	return reader.reader.Read(data)
}

func firstNonNil(primary, fallback error) error {
	if primary != nil {
		return primary
	}
	return fallback
}

func remoteDownloadBase64Command(target SSHTarget, workdir, remotePath string) string {
	if isWindowsNativeTarget(target) {
		return powershellCommand(`$ErrorActionPreference = "Stop"
Set-Location -LiteralPath ` + psQuote(workdir) + `
$path = ` + psQuote(remotePath) + `
if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "download file not found: $path" }
$file = [System.IO.File]::OpenRead((Resolve-Path -LiteralPath $path).Path)
try {
  [Console]::Out.WriteLine("` + remoteDownloadHeaderPrefix + `{0}", $file.Length)
  $stdout = [Console]::OpenStandardOutput()
  $transform = [System.Security.Cryptography.ToBase64Transform]::new()
  $encoded = [System.Security.Cryptography.CryptoStream]::new($stdout, $transform, [System.Security.Cryptography.CryptoStreamMode]::Write, $true)
  try {
    $file.CopyTo($encoded)
    $encoded.FlushFinalBlock()
  } finally {
    $encoded.Dispose()
    $transform.Dispose()
  }
} finally {
  $file.Dispose()
}`)
	}
	return fmt.Sprintf(
		"cd %s && test -f %s && size=$(LC_ALL=C wc -c < %s) && printf '%s%%s\\n' \"$size\" && base64 < %s",
		shellQuote(workdir),
		shellQuote(remotePath),
		shellQuote(remotePath),
		remoteDownloadHeaderPrefix,
		shellQuote(remotePath),
	)
}
