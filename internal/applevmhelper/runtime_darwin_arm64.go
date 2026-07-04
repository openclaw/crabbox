//go:build darwin && arm64

package applevmhelper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lima-vm/go-qcow2reader"
	"golang.org/x/sys/unix"
)

type startConfig struct {
	StateRoot    string
	Instance     Instance
	Image        string
	ImageSHA256  string
	SSHPublicKey string
}

const maxRemoteImageBytes int64 = 32 << 30

const (
	qcow2HeaderV2Bytes       = 72
	qcow2HeaderV3Bytes       = 104
	qcow2MaxL1TableBytes     = 32 << 20
	qcow2MaxRefcountBytes    = 8 << 20
	qcow2MinSnapshotHdrBytes = 40
	maxVSOCKProxyChannels    = 32
	seedImageCommandTimeout  = 10 * time.Second
	seedImageCleanupTimeout  = 15 * time.Second
)

var execSeedImageCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func prepareInstanceAssets(ctx context.Context, cfg startConfig) (Instance, error) {
	inst := cfg.Instance
	if err := ensurePrivateDir(InstanceDir(cfg.StateRoot, inst.Name)); err != nil {
		return Instance{}, fmt.Errorf("create instance directory: %w", err)
	}
	if err := cleanupImageCacheTemps(cfg.StateRoot); err != nil {
		return Instance{}, err
	}
	targetSize, err := diskCapacityBytes(inst.DiskGiB)
	if err != nil {
		return Instance{}, err
	}
	sourcePath, err := resolveSourceImageWithLimit(ctx, cfg.StateRoot, cfg.Image, cfg.ImageSHA256, targetSize)
	if err != nil {
		return Instance{}, err
	}
	rawPath, err := ensureRawImage(ctx, cfg.StateRoot, cfg.Image, sourcePath, cfg.ImageSHA256, targetSize)
	if err != nil {
		return Instance{}, err
	}
	inst.SourceImage = sourcePath
	inst.DiskPath = DiskPath(cfg.StateRoot, inst.Name)
	inst.SeedPath = SeedPath(cfg.StateRoot, inst.Name)
	inst.EFIVariableStorePath = EFIPath(cfg.StateRoot, inst.Name)
	inst.ConsoleLogPath = ConsoleLogPath(cfg.StateRoot, inst.Name)
	if err := cloneOrCopyFile(ctx, rawPath, inst.DiskPath); err != nil {
		return Instance{}, fmt.Errorf("clone base disk: %w", err)
	}
	if err := os.Chmod(inst.DiskPath, 0o600); err != nil {
		return Instance{}, fmt.Errorf("secure root disk: %w", err)
	}
	info, err := os.Stat(inst.DiskPath)
	if err != nil {
		return Instance{}, fmt.Errorf("stat cloned disk: %w", err)
	}
	if info.Size() < targetSize {
		if err := os.Truncate(inst.DiskPath, targetSize); err != nil {
			return Instance{}, fmt.Errorf("resize disk: %w", err)
		}
	}
	if err := createSeedImage(ctx, inst.SeedPath, inst.Name, inst.SSHUser, cfg.SSHPublicKey, inst.WorkRoot); err != nil {
		return Instance{}, err
	}
	if err := ensurePrivateDir(filepath.Dir(inst.ConsoleLogPath)); err != nil {
		return Instance{}, fmt.Errorf("create console log directory: %w", err)
	}
	if file, err := os.OpenFile(inst.ConsoleLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		_ = file.Chmod(0o600)
		_ = file.Close()
	}
	return inst, nil
}

func resolveSourceImage(ctx context.Context, stateRoot, image, expectedSHA256 string) (string, error) {
	return resolveSourceImageWithLimit(ctx, stateRoot, image, expectedSHA256, math.MaxInt64)
}

func resolveSourceImageWithLimit(ctx context.Context, stateRoot, image, expectedSHA256 string, maxDiskBytes int64) (string, error) {
	image = strings.TrimSpace(image)
	if image == "" {
		return "", fmt.Errorf("image is required")
	}
	checksum, err := normalizeExpectedSHA256(expectedSHA256)
	if err != nil {
		return "", err
	}
	parsedURL, remote, err := validateRemoteImageURL(image)
	if err != nil {
		return "", err
	}
	if remote {
		displayImage := RedactImageRef(image)
		if checksum == "" {
			return "", fmt.Errorf("apple-vm remote image %q requires a SHA-256 checksum", displayImage)
		}
		if err := ensurePrivateDir(DownloadsDir(stateRoot)); err != nil {
			return "", fmt.Errorf("create downloads cache: %w", err)
		}
		sum := sha256.Sum256([]byte(image + "\x00" + checksum))
		target := filepath.Join(DownloadsDir(stateRoot), hex.EncodeToString(sum[:8])+"-image.img")
		if _, err := os.Stat(target); err == nil {
			if err := os.Chmod(target, 0o600); err != nil {
				return "", fmt.Errorf("secure cached image: %w", err)
			}
			if err := verifyFileSHA256(target, checksum); err != nil {
				return "", err
			}
			return target, nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
		if err != nil {
			return "", fmt.Errorf("build image request for %q: invalid URL", displayImage)
		}
		req.Header.Set("User-Agent", "crabbox-apple-vm-helper")
		originLoopback := isLoopbackImageHost(parsedURL.Hostname())
		client := &http.Client{
			Timeout: 2 * time.Hour,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				req.Header.Del("Referer")
				if len(via) >= 10 {
					return fmt.Errorf("stopped after 10 redirects")
				}
				return validateImageRedirect(req.URL, originLoopback)
			},
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("download image %q: %s", displayImage, safeDownloadError(err))
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("download image %q: http %d", displayImage, resp.StatusCode)
		}
		if resp.ContentLength > maxRemoteImageBytes {
			return "", fmt.Errorf("download image %q: content length exceeds 32 GiB limit", displayImage)
		}
		file, err := createCacheTemp(target)
		if err != nil {
			return "", fmt.Errorf("create image cache file: %w", err)
		}
		tmp := file.Name()
		defer file.Close()
		defer os.Remove(tmp)
		if err := file.Chmod(0o600); err != nil {
			file.Close()
			return "", fmt.Errorf("set image cache permissions: %w", err)
		}
		actual, err := copyRemoteImage(file, resp.Body, maxRemoteImageBytes)
		if err != nil {
			file.Close()
			return "", fmt.Errorf("write image cache file: %w", err)
		}
		if err := file.Sync(); err != nil {
			return "", fmt.Errorf("sync image cache file: %w", err)
		}
		if actual != checksum {
			return "", fmt.Errorf("verify image %q: sha256 %s does not match expected %s", displayImage, actual, checksum)
		}
		if err := os.Rename(tmp, target); err != nil {
			return "", fmt.Errorf("commit image cache file: %w", err)
		}
		if err := file.Close(); err != nil {
			return "", fmt.Errorf("close image cache file: %w", err)
		}
		return target, nil
	}
	path, err := resolveLocalImagePath(image)
	if err != nil {
		return "", err
	}
	if checksum != "" {
		return cacheVerifiedLocalImage(ctx, stateRoot, path, checksum, maxDiskBytes)
	}
	return path, nil
}

func resolveLocalImagePath(image string) (string, error) {
	path := strings.TrimSpace(image)
	if strings.HasPrefix(path, "~"+string(os.PathSeparator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve image path: %w", err)
		}
		path = abs
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("image %q: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return "", fmt.Errorf("image %q: invalid file descriptor", path)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("stat image %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("image %q is not a regular file", path)
	}
	return path, nil
}

func validateImageRedirect(target *url.URL, originLoopback bool) error {
	target.Scheme = strings.ToLower(target.Scheme)
	if target.Scheme != "http" && target.Scheme != "https" {
		return fmt.Errorf("unsupported redirect scheme")
	}
	targetLoopback := isLoopbackImageHost(target.Hostname())
	if targetLoopback != originLoopback {
		return fmt.Errorf("redirect crosses the loopback trust boundary")
	}
	if target.Scheme == "http" && !targetLoopback {
		return fmt.Errorf("redirect to non-loopback HTTP is not allowed")
	}
	return nil
}

func cacheVerifiedLocalImage(ctx context.Context, stateRoot, sourcePath, checksum string, maxDiskBytes int64) (string, error) {
	if err := ensurePrivateDir(DownloadsDir(stateRoot)); err != nil {
		return "", fmt.Errorf("create downloads cache: %w", err)
	}
	target := filepath.Join(DownloadsDir(stateRoot), "local-"+checksum[:16]+"-image.img")
	if _, err := os.Stat(target); err == nil {
		if err := os.Chmod(target, 0o600); err != nil {
			return "", fmt.Errorf("secure cached image: %w", err)
		}
		if err := verifyFileSHA256(target, checksum); err != nil {
			return "", err
		}
		if err := validateImageDiskSize(target, maxDiskBytes); err != nil {
			return "", err
		}
		return target, nil
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return "", fmt.Errorf("open image %q: %w", sourcePath, err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return "", fmt.Errorf("stat image %q: %w", sourcePath, err)
	}
	qcow2, err := readerIsQCOW2(source)
	if err != nil {
		return "", err
	}
	if !qcow2 {
		err = validateSourceDiskSize(info.Size(), maxDiskBytes)
	}
	if err != nil {
		return "", err
	}
	file, err := createCacheTemp(target)
	if err != nil {
		return "", fmt.Errorf("create image cache file: %w", err)
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	file, cloned, err := replaceCacheTempWithClone(source, file)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var actual string
	if cloned {
		actual, err = fileSHA256(tmp)
	} else {
		actual, err = copySparseImageWithSHA256(ctx, file, source, info.Size())
	}
	if err != nil {
		return "", fmt.Errorf("copy local image: %w", err)
	}
	if actual != checksum {
		return "", fmt.Errorf("verify image %q: sha256 %s does not match expected %s", sourcePath, actual, checksum)
	}
	if err := syncFile(tmp); err != nil {
		return "", fmt.Errorf("sync image cache file: %w", err)
	}
	if err := validateImageDiskSize(tmp, maxDiskBytes); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", fmt.Errorf("commit image cache file: %w", err)
	}
	return target, nil
}

func replaceCacheTempWithClone(source, temp *os.File) (*os.File, bool, error) {
	path := temp.Name()
	dirLock, err := lockCacheDir(filepath.Dir(path))
	if err != nil {
		return nil, false, err
	}
	fail := func(err error) (*os.File, bool, error) {
		return nil, false, errors.Join(err, unlockCacheDir(dirLock))
	}
	if err := temp.Close(); err != nil {
		return fail(fmt.Errorf("close image cache placeholder: %w", err))
	}
	if err := os.Remove(path); err != nil {
		return fail(fmt.Errorf("remove image cache placeholder: %w", err))
	}
	cloned, err := cloneLocalImage(source, path)
	if err != nil {
		return fail(err)
	}
	if cloned {
		if err := os.Chmod(path, 0o600); err != nil {
			return fail(fmt.Errorf("secure cloned image: %w", err))
		}
	}
	flags := os.O_RDWR
	if !cloned {
		flags |= os.O_CREATE | os.O_EXCL
	}
	replacement, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return fail(fmt.Errorf("open image cache staging file: %w", err))
	}
	if err := replacement.Chmod(0o600); err != nil {
		replacement.Close()
		return fail(fmt.Errorf("secure image cache staging file: %w", err))
	}
	if err := unix.Flock(int(replacement.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		replacement.Close()
		return fail(fmt.Errorf("lock image cache staging file: %w", err))
	}
	if err := unlockCacheDir(dirLock); err != nil {
		replacement.Close()
		return nil, false, err
	}
	return replacement, cloned, nil
}

func cloneLocalImage(source *os.File, targetPath string) (bool, error) {
	err := unix.Fclonefileat(int(source.Fd()), unix.AT_FDCWD, targetPath, unix.CLONE_NOOWNERCOPY)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EINVAL) {
		return false, nil
	}
	return false, fmt.Errorf("clone local image: %w", err)
}

func syncFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

type sparseFileWriter interface {
	io.WriterAt
	Truncate(size int64) error
}

func copySparseImageWithSHA256(ctx context.Context, target sparseFileWriter, source io.ReaderAt, size int64) (string, error) {
	if err := target.Truncate(size); err != nil {
		return "", err
	}
	hash := sha256.New()
	buf := make([]byte, 4*1024*1024)
	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		chunk := int64(len(buf))
		if remaining := size - offset; remaining < chunk {
			chunk = remaining
		}
		n, err := source.ReadAt(buf[:chunk], offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		if n == 0 {
			return "", io.ErrUnexpectedEOF
		}
		part := buf[:n]
		if _, err := hash.Write(part); err != nil {
			return "", err
		}
		if err := writeSparseBlocks(target, part, offset); err != nil {
			return "", err
		}
		offset += int64(n)
		if errors.Is(err, io.EOF) && offset < size {
			return "", io.ErrUnexpectedEOF
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyRemoteImage(target io.Writer, source io.Reader, maxBytes int64) (string, error) {
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(target, hash), io.LimitReader(source, maxBytes+1))
	if err != nil {
		return "", err
	}
	if written > maxBytes {
		return "", fmt.Errorf("download exceeds 32 GiB limit")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func safeDownloadError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "request canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "request timed out"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "request timed out"
	}
	return "request failed"
}

func isLoopbackImageHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateRemoteImageURL(image string) (*url.URL, bool, error) {
	parsed, remote := remoteImageURL(image)
	if !remote {
		return nil, false, nil
	}
	if parsed == nil || parsed.Host == "" {
		return nil, true, fmt.Errorf("parse image URL %q: invalid URL", RedactImageRef(image))
	}
	if parsed.Scheme == "http" && !isLoopbackImageHost(parsed.Hostname()) {
		return nil, true, fmt.Errorf("apple-vm remote images must use HTTPS; HTTP is allowed only for loopback development")
	}
	return parsed, true, nil
}

func normalizeExpectedSHA256(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return "", nil
	}
	if strings.HasPrefix(normalized, "sha256:") {
		normalized = strings.TrimPrefix(normalized, "sha256:")
	}
	if len(normalized) != sha256.Size*2 {
		return "", fmt.Errorf("image-sha256 must be a 64-character SHA-256 digest")
	}
	if _, err := hex.DecodeString(normalized); err != nil {
		return "", fmt.Errorf("image-sha256 must be hex: %w", err)
	}
	return normalized, nil
}

func verifyFileSHA256(path, expected string) error {
	actual, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("verify image %q: sha256 %s does not match expected %s", path, actual, expected)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open image for sha256: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash image: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func ensureRawImage(ctx context.Context, stateRoot, sourceRef, sourcePath, expectedSHA256 string, maxDiskBytes int64) (string, error) {
	qcow2, err := isQCOW2(sourcePath)
	if err != nil {
		return "", err
	}
	if !qcow2 {
		info, err := os.Stat(sourcePath)
		if err != nil {
			return "", fmt.Errorf("stat source image: %w", err)
		}
		if err := validateSourceDiskSize(info.Size(), maxDiskBytes); err != nil {
			return "", err
		}
		return sourcePath, nil
	}
	if err := validateStandaloneQCOW2Path(sourcePath, maxDiskBytes); err != nil {
		return "", err
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		return "", fmt.Errorf("stat source image: %w", err)
	}
	if err := ensurePrivateDir(ImagesDir(stateRoot)); err != nil {
		return "", fmt.Errorf("create image cache: %w", err)
	}
	checksum, err := normalizeExpectedSHA256(expectedSHA256)
	if err != nil {
		return "", err
	}
	key := rawImageCacheKey(sourceRef, sourcePath, checksum, info)
	target := filepath.Join(ImagesDir(stateRoot), hex.EncodeToString(key[:])+".raw")
	if cachedInfo, err := os.Stat(target); err == nil {
		if err := validateSourceDiskSize(cachedInfo.Size(), maxDiskBytes); err != nil {
			return "", err
		}
		if err := os.Chmod(target, 0o600); err != nil {
			return "", fmt.Errorf("secure cached raw image: %w", err)
		}
		return target, nil
	}
	tmpFile, err := createCacheTemp(target)
	if err != nil {
		return "", fmt.Errorf("create raw image staging file: %w", err)
	}
	tmp := tmpFile.Name()
	defer tmpFile.Close()
	defer os.Remove(tmp)
	if err := convertQCOW2ToRaw(ctx, sourcePath, tmpFile, maxDiskBytes); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", fmt.Errorf("commit raw image: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close raw image: %w", err)
	}
	return target, nil
}

func validateStandaloneQCOW2Path(sourcePath string, maxDiskBytes int64) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open qcow2 image: %w", err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return fmt.Errorf("stat qcow2 image: %w", err)
	}
	if err := validateQCOW2Metadata(source, info.Size(), maxDiskBytes); err != nil {
		return err
	}
	_, err = newStandaloneQCOW2Reader(source)
	return err
}

func rawImageCacheKey(sourceRef, sourcePath, checksum string, info os.FileInfo) [sha256.Size]byte {
	identity := ""
	if checksum == "" {
		identity = sourceFileIdentity(info)
	}
	return sha256.Sum256([]byte(fmt.Sprintf(
		"%s|%s|%s|%d|%d|%s",
		sourceRef,
		sourcePath,
		checksum,
		info.Size(),
		info.ModTime().UnixNano(),
		identity,
	)))
}

func sourceFileIdentity(info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d:%d", stat.Dev, stat.Ino, stat.Ctimespec.Sec, stat.Ctimespec.Nsec)
}

func createCacheTemp(target string) (*os.File, error) {
	dir := filepath.Dir(target)
	dirLock, err := lockCacheDir(dir)
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(dir, "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return nil, errors.Join(err, unlockCacheDir(dirLock))
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		_ = os.Remove(file.Name())
		return nil, errors.Join(err, unlockCacheDir(dirLock))
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		_ = os.Remove(file.Name())
		return nil, errors.Join(err, unlockCacheDir(dirLock))
	}
	if err := unlockCacheDir(dirLock); err != nil {
		file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	return file, nil
}

func cleanupImageCacheTemps(stateRoot string) error {
	for _, cacheDir := range []struct {
		label string
		path  string
	}{
		{label: "downloads", path: DownloadsDir(stateRoot)},
		{label: "image", path: ImagesDir(stateRoot)},
	} {
		if err := ensurePrivateDir(cacheDir.path); err != nil {
			return fmt.Errorf("create %s cache: %w", cacheDir.label, err)
		}
		if err := cleanupAbandonedCacheTemps(cacheDir.path); err != nil {
			return fmt.Errorf("clean %s cache: %w", cacheDir.label, err)
		}
	}
	return nil
}

func lockCacheDir(dir string) (*os.File, error) {
	path := filepath.Join(dir, ".staging.lock")
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func unlockCacheDir(file *os.File) error {
	if file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}

func cleanupAbandonedCacheTemps(dir string) (returnErr error) {
	dirLock, err := lockCacheDir(dir)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, unlockCacheDir(dirLock))
	}()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ".") || !strings.Contains(name, ".tmp-") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(dir, name)
		fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) {
			continue
		}
		if err != nil {
			return err
		}
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			_ = unix.Close(fd)
			continue
		} else if err != nil {
			_ = unix.Close(fd)
			return err
		}
		removeErr := os.Remove(path)
		unlockErr := unix.Flock(fd, unix.LOCK_UN)
		closeErr := unix.Close(fd)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
		if unlockErr != nil {
			return unlockErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func isQCOW2(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open image: %w", err)
	}
	defer file.Close()
	return readerIsQCOW2(file)
}

func readerIsQCOW2(source io.ReaderAt) (bool, error) {
	var header [4]byte
	if _, err := io.ReadFull(io.NewSectionReader(source, 0, int64(len(header))), header[:]); err != nil {
		return false, fmt.Errorf("read image header: %w", err)
	}
	return bytes.Equal(header[:], []byte{'Q', 'F', 'I', 0xfb}), nil
}

func convertQCOW2ToRaw(ctx context.Context, sourcePath string, target *os.File, maxDiskBytes int64) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open qcow2 image: %w", err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return fmt.Errorf("stat qcow2 image: %w", err)
	}
	if err := validateQCOW2Metadata(source, info.Size(), maxDiskBytes); err != nil {
		return err
	}
	reader, err := newStandaloneQCOW2Reader(source)
	if err != nil {
		return err
	}
	image, err := qcow2reader.Open(reader)
	if err != nil {
		return fmt.Errorf("open qcow2 image: %w", err)
	}
	defer image.Close()
	if err := image.Readable(); err != nil {
		return fmt.Errorf("qcow2 image not readable: %w", err)
	}
	size := image.Size()
	if size <= 0 {
		return fmt.Errorf("qcow2 image size is unavailable")
	}
	if err := validateSourceDiskSize(size, maxDiskBytes); err != nil {
		return err
	}
	if err := target.Truncate(size); err != nil {
		return fmt.Errorf("size raw image: %w", err)
	}
	buf := make([]byte, 4*1024*1024)
	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return err
		}
		extent, err := image.Extent(offset, size-offset)
		if err != nil {
			return fmt.Errorf("read qcow2 extent at %d: %w", offset, err)
		}
		end := extent.Start + extent.Length
		if end <= offset {
			return fmt.Errorf("invalid qcow2 extent at %d", offset)
		}
		start := offset
		if extent.Start > start {
			start = extent.Start
		}
		if extent.Allocated && !extent.Zero {
			if err := copyReaderAtRange(ctx, target, image, start, end-start, buf); err != nil {
				return fmt.Errorf("copy qcow2 extent at %d: %w", start, err)
			}
		}
		offset = end
	}
	return target.Sync()
}

type standaloneQCOW2Reader struct {
	source io.ReaderAt
	header [20]byte
}

func newStandaloneQCOW2Reader(source io.ReaderAt) (*standaloneQCOW2Reader, error) {
	reader := &standaloneQCOW2Reader{source: source}
	if _, err := io.ReadFull(io.NewSectionReader(source, 0, int64(len(reader.header))), reader.header[:]); err != nil {
		return nil, fmt.Errorf("read qcow2 header: %w", err)
	}
	if !bytes.Equal(reader.header[:4], []byte{'Q', 'F', 'I', 0xfb}) {
		return nil, fmt.Errorf("invalid qcow2 magic")
	}
	backingOffset := binary.BigEndian.Uint64(reader.header[8:16])
	backingSize := binary.BigEndian.Uint32(reader.header[16:20])
	if backingOffset != 0 || backingSize != 0 {
		return nil, fmt.Errorf("qcow2 backing files are not supported")
	}
	return reader, nil
}

func validateQCOW2Metadata(source io.ReaderAt, sourceBytes, maxDiskBytes int64) error {
	if sourceBytes < qcow2HeaderV2Bytes {
		return fmt.Errorf("qcow2 image is shorter than the %d-byte header", qcow2HeaderV2Bytes)
	}
	header := make([]byte, qcow2HeaderV3Bytes)
	if _, err := source.ReadAt(header[:qcow2HeaderV2Bytes], 0); err != nil {
		return fmt.Errorf("read qcow2 header: %w", err)
	}
	if !bytes.Equal(header[:4], []byte{'Q', 'F', 'I', 0xfb}) {
		return fmt.Errorf("invalid qcow2 magic")
	}
	version := binary.BigEndian.Uint32(header[4:8])
	if version != 2 && version != 3 {
		return fmt.Errorf("unsupported qcow2 version %d", version)
	}
	if binary.BigEndian.Uint64(header[8:16]) != 0 || binary.BigEndian.Uint32(header[16:20]) != 0 {
		return fmt.Errorf("qcow2 backing files are not supported")
	}
	clusterBits := binary.BigEndian.Uint32(header[20:24])
	if clusterBits < 9 || clusterBits > 21 {
		return fmt.Errorf("qcow2 cluster bits %d are outside the supported range 9..21", clusterBits)
	}
	clusterBytes := uint64(1) << clusterBits
	virtualBytes := binary.BigEndian.Uint64(header[24:32])
	if virtualBytes == 0 || virtualBytes > math.MaxInt64 {
		return fmt.Errorf("invalid qcow2 virtual disk size %d bytes", virtualBytes)
	}
	if err := validateSourceDiskSize(int64(virtualBytes), maxDiskBytes); err != nil {
		return err
	}
	if cryptMethod := binary.BigEndian.Uint32(header[32:36]); cryptMethod != 0 {
		return fmt.Errorf("encrypted qcow2 images are not supported")
	}

	incompatibleFeatures := uint64(0)
	headerBytes := uint64(qcow2HeaderV2Bytes)
	if version == 3 {
		if sourceBytes < qcow2HeaderV3Bytes {
			return fmt.Errorf("qcow2 v3 image is shorter than the %d-byte header", qcow2HeaderV3Bytes)
		}
		if _, err := source.ReadAt(header[qcow2HeaderV2Bytes:], qcow2HeaderV2Bytes); err != nil {
			return fmt.Errorf("read qcow2 v3 header: %w", err)
		}
		incompatibleFeatures = binary.BigEndian.Uint64(header[72:80])
		if incompatibleFeatures&(1<<1) != 0 {
			return fmt.Errorf("qcow2 image is marked corrupt")
		}
		if incompatibleFeatures&^uint64(0x1b) != 0 || incompatibleFeatures&(1<<2) != 0 {
			return fmt.Errorf("unsupported qcow2 incompatible features 0x%x", incompatibleFeatures)
		}
		if refcountOrder := binary.BigEndian.Uint32(header[96:100]); refcountOrder > 6 {
			return fmt.Errorf("qcow2 refcount order %d exceeds 6", refcountOrder)
		}
		headerBytes = uint64(binary.BigEndian.Uint32(header[100:104]))
		if headerBytes < qcow2HeaderV3Bytes || headerBytes%8 != 0 || headerBytes > clusterBytes {
			return fmt.Errorf("invalid qcow2 header length %d", headerBytes)
		}
		if incompatibleFeatures&(1<<4) != 0 && clusterBits < 14 {
			return fmt.Errorf("qcow2 extended L2 entries require cluster bits >= 14")
		}
	}

	l1Entries := uint64(binary.BigEndian.Uint32(header[36:40]))
	l1Bytes := l1Entries * 8
	if l1Entries == 0 || l1Bytes > qcow2MaxL1TableBytes {
		return fmt.Errorf("qcow2 L1 table size %d bytes exceeds the %d-byte limit", l1Bytes, qcow2MaxL1TableBytes)
	}
	l2EntryBytes := uint64(8)
	if incompatibleFeatures&(1<<4) != 0 {
		l2EntryBytes = 16
	}
	coveragePerL1Entry := clusterBytes * (clusterBytes / l2EntryBytes)
	requiredL1Entries := (virtualBytes + coveragePerL1Entry - 1) / coveragePerL1Entry
	if l1Entries < requiredL1Entries {
		return fmt.Errorf("qcow2 L1 table has %d entries; virtual disk requires at least %d", l1Entries, requiredL1Entries)
	}
	if err := validateQCOW2Range("L1 table", binary.BigEndian.Uint64(header[40:48]), l1Bytes, clusterBytes, sourceBytes); err != nil {
		return err
	}

	refcountClusters := uint64(binary.BigEndian.Uint32(header[56:60]))
	refcountBytes := refcountClusters * clusterBytes
	if refcountClusters == 0 || refcountBytes > qcow2MaxRefcountBytes {
		return fmt.Errorf("qcow2 refcount table size %d bytes exceeds the %d-byte limit", refcountBytes, qcow2MaxRefcountBytes)
	}
	if err := validateQCOW2Range("refcount table", binary.BigEndian.Uint64(header[48:56]), refcountBytes, clusterBytes, sourceBytes); err != nil {
		return err
	}

	snapshots := uint64(binary.BigEndian.Uint32(header[60:64]))
	if snapshots > 0 {
		snapshotBytes := snapshots * qcow2MinSnapshotHdrBytes
		if err := validateQCOW2Range("snapshot table", binary.BigEndian.Uint64(header[64:72]), snapshotBytes, clusterBytes, sourceBytes); err != nil {
			return err
		}
	}
	return validateQCOW2HeaderExtensions(source, headerBytes, clusterBytes, sourceBytes)
}

func validateQCOW2Range(label string, offset, length, alignment uint64, sourceBytes int64) error {
	if offset == 0 || offset%alignment != 0 {
		return fmt.Errorf("qcow2 %s offset %d is not cluster-aligned", label, offset)
	}
	limit := uint64(sourceBytes)
	if offset > limit || length > limit-offset {
		return fmt.Errorf("qcow2 %s range [%d,%d) exceeds the %d-byte image", label, offset, offset+length, sourceBytes)
	}
	return nil
}

func validateQCOW2HeaderExtensions(source io.ReaderAt, offset, clusterBytes uint64, sourceBytes int64) error {
	limit := min(clusterBytes, uint64(sourceBytes))
	var extensionHeader [8]byte
	for offset+uint64(len(extensionHeader)) <= limit {
		if _, err := source.ReadAt(extensionHeader[:], int64(offset)); err != nil {
			return fmt.Errorf("read qcow2 header extension: %w", err)
		}
		extensionType := binary.BigEndian.Uint32(extensionHeader[:4])
		extensionBytes := uint64(binary.BigEndian.Uint32(extensionHeader[4:]))
		if extensionType == 0 {
			if extensionBytes != 0 {
				return fmt.Errorf("qcow2 end header extension has nonzero length %d", extensionBytes)
			}
			return nil
		}
		paddedBytes := (extensionBytes + 7) &^ uint64(7)
		offset += uint64(len(extensionHeader))
		if paddedBytes > limit-offset {
			return fmt.Errorf("qcow2 header extension exceeds the first cluster")
		}
		offset += paddedBytes
	}
	return fmt.Errorf("qcow2 header extensions are missing an end marker")
}

func (r *standaloneQCOW2Reader) ReadAt(p []byte, offset int64) (int, error) {
	n, err := r.source.ReadAt(p, offset)
	if n == 0 || offset >= int64(len(r.header)) || offset+int64(n) <= 0 {
		return n, err
	}
	start := max(offset, 0)
	end := min(offset+int64(n), int64(len(r.header)))
	copy(p[start-offset:end-offset], r.header[start:end])
	return n, err
}

func diskCapacityBytes(diskGiB int) (int64, error) {
	if diskGiB <= 0 || int64(diskGiB) > math.MaxInt64/(1<<30) {
		return 0, fmt.Errorf("invalid disk size %d GiB", diskGiB)
	}
	return int64(diskGiB) << 30, nil
}

func validateSourceDiskSize(sourceBytes, maxDiskBytes int64) error {
	if sourceBytes > maxDiskBytes {
		return fmt.Errorf("source image size %d bytes exceeds configured disk size %d bytes", sourceBytes, maxDiskBytes)
	}
	return nil
}

func validateImageDiskSize(sourcePath string, maxDiskBytes int64) error {
	qcow2, err := isQCOW2(sourcePath)
	if err != nil {
		return err
	}
	if !qcow2 {
		info, err := os.Stat(sourcePath)
		if err != nil {
			return fmt.Errorf("stat source image: %w", err)
		}
		return validateSourceDiskSize(info.Size(), maxDiskBytes)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open qcow2 image: %w", err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return fmt.Errorf("stat qcow2 image: %w", err)
	}
	if err := validateQCOW2Metadata(source, info.Size(), maxDiskBytes); err != nil {
		return err
	}
	reader, err := newStandaloneQCOW2Reader(source)
	if err != nil {
		return err
	}
	image, err := qcow2reader.Open(reader)
	if err != nil {
		return fmt.Errorf("open qcow2 image: %w", err)
	}
	defer image.Close()
	if err := image.Readable(); err != nil {
		return fmt.Errorf("qcow2 image not readable: %w", err)
	}
	return validateSourceDiskSize(image.Size(), maxDiskBytes)
}

func cloneOrCopyFile(ctx context.Context, sourcePath, targetPath string) error {
	_ = os.Remove(targetPath)
	if err := unix.Clonefile(sourcePath, targetPath, 0); err == nil {
		return nil
	} else if !errors.Is(err, unix.EXDEV) && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EPERM) && !errors.Is(err, syscall.ENOTSUP) {
		return fmt.Errorf("clone file: %w", err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open source file: %w", err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return fmt.Errorf("stat source file: %w", err)
	}
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create target file: %w", err)
	}
	if err := target.Truncate(info.Size()); err != nil {
		return errors.Join(fmt.Errorf("size target file: %w", err), target.Close())
	}
	buf := make([]byte, 4*1024*1024)
	if err := copyReaderAtRange(ctx, target, source, 0, info.Size(), buf); err != nil {
		return errors.Join(err, target.Close())
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("close target file: %w", err)
	}
	return nil
}

func copyReaderAtRange(ctx context.Context, target *os.File, source io.ReaderAt, offset, length int64, buf []byte) error {
	for copied := int64(0); copied < length; {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := int64(len(buf))
		if remaining := length - copied; remaining < chunk {
			chunk = remaining
		}
		n, err := source.ReadAt(buf[:chunk], offset+copied)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
		part := buf[:n]
		if err := writeSparseBlocks(target, part, offset+copied); err != nil {
			return err
		}
		copied += int64(n)
		if errors.Is(err, io.EOF) && copied < length {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

func writeSparseBlocks(target io.WriterAt, data []byte, offset int64) error {
	const blockSize = 4 * 1024
	for start := 0; start < len(data); {
		for start < len(data) {
			end := min(start+blockSize, len(data))
			if !allZero(data[start:end]) {
				break
			}
			start = end
		}
		if start == len(data) {
			break
		}
		end := start
		for end < len(data) {
			next := min(end+blockSize, len(data))
			if allZero(data[end:next]) {
				break
			}
			end = next
		}
		written, err := target.WriteAt(data[start:end], offset+int64(start))
		if err != nil {
			return err
		}
		if written != end-start {
			return io.ErrShortWrite
		}
		start = end
	}
	return nil
}

func allZero(buf []byte) bool {
	for _, b := range buf {
		if b != 0 {
			return false
		}
	}
	return true
}

func createSeedImage(ctx context.Context, path, hostName, user, publicKey, workRoot string) (returnErr error) {
	tmpDir, err := os.MkdirTemp("", "crabbox-apple-vm-seed-*")
	if err != nil {
		return fmt.Errorf("create seed temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	if err := os.WriteFile(filepath.Join(tmpDir, "meta-data"), []byte(fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", hostName, hostName)), 0o644); err != nil {
		return fmt.Errorf("write seed meta-data: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "user-data"), []byte(seedUserData(user, publicKey, workRoot)), 0o644); err != nil {
		return fmt.Errorf("write seed user-data: %w", err)
	}
	_ = os.Remove(path)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create seed image: %w", err)
	}
	if err := file.Truncate(8 * 1024 * 1024); err != nil {
		return errors.Join(fmt.Errorf("size seed image: %w", err), file.Close())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("create seed image: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Finish the bounded attach after cancellation so any returned device can
	// be registered and detached before the helper exits.
	attachOut, err := runSeedImageCommand(context.WithoutCancel(ctx), "hdiutil", "attach", "-imagekey", "diskimage-class=CRawDiskImage", "-nomount", path)
	device := attachedDevice(string(attachOut))
	if err != nil {
		attachErr := fmt.Errorf("attach seed image: %w: %s", err, strings.TrimSpace(string(attachOut)))
		if device == "" {
			return attachErr
		}
		return errors.Join(attachErr, cleanupSeedImage("", device, false))
	}
	if device == "" {
		return fmt.Errorf("attach seed image: missing device name")
	}
	mounted := false
	mountDir := ""
	defer func() {
		returnErr = errors.Join(returnErr, cleanupSeedImage(mountDir, device, mounted))
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if out, err := runSeedImageCommand(ctx, "newfs_msdos", "-F", "16", "-v", "cidata", device); err != nil {
		return fmt.Errorf("format seed image: %w: %s", err, strings.TrimSpace(string(out)))
	}
	mountDir = filepath.Join(tmpDir, "mnt")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		return fmt.Errorf("create seed mount dir: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Finish the bounded mount after cancellation so deferred cleanup always
	// knows whether an unmount is required.
	out, err := runSeedImageCommand(context.WithoutCancel(ctx), "mount", "-t", "msdos", device, mountDir)
	mounted = true
	if err != nil {
		return fmt.Errorf("mount seed image: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, name := range []string{"meta-data", "user-data"} {
		if err := copyPlainFile(filepath.Join(tmpDir, name), filepath.Join(mountDir, name), 0o644); err != nil {
			return fmt.Errorf("populate seed image: %w", err)
		}
	}
	return nil
}

func runSeedImageCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, seedImageCommandTimeout)
	defer cancel()
	return execSeedImageCommand(commandCtx, name, args...)
}

func cleanupSeedImage(mountDir, device string, mounted bool) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), seedImageCleanupTimeout)
	defer cancel()
	var cleanupErrors []error
	if mounted {
		if out, err := runSeedImageCommand(cleanupCtx, "umount", mountDir); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("unmount seed image: %w: %s", err, strings.TrimSpace(string(out))))
		}
	}
	if out, err := runSeedImageCommand(cleanupCtx, "hdiutil", "detach", device); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("detach seed image: %w: %s", err, strings.TrimSpace(string(out))))
	}
	return errors.Join(cleanupErrors...)
}

func seedUserData(user, publicKey, workRoot string) string {
	return strings.TrimSpace(fmt.Sprintf(`
#cloud-config
users:
  - default
  - name: %s
    gecos: Crabbox
    shell: /bin/bash
    lock_passwd: true
    sudo: ALL=(ALL) NOPASSWD:ALL
    groups: [adm, sudo]
    ssh_authorized_keys:
      - %s
write_files:
  - path: /usr/local/bin/crabbox-vsock-ssh-proxy.py
    owner: root:root
    permissions: "0755"
    content: |
%s
  - path: /etc/systemd/system/crabbox-vsock-ssh-proxy.service
    owner: root:root
    permissions: "0644"
    content: |
%s
  - path: /usr/local/bin/crabbox-ready
    owner: root:root
    permissions: "0755"
    content: |
%s
runcmd:
  - [mkdir, -p, %s]
  - [chown, %s, %s]
  - [systemctl, daemon-reload]
  - [systemctl, enable, --now, crabbox-vsock-ssh-proxy.service]
`, yamlString(user), yamlString(publicKey), indentBlock(vsockProxyPython), indentBlock(vsockProxyService), indentBlock(readyScript(workRoot)), yamlString(workRoot), yamlString(user+":"+user), yamlString(workRoot))) + "\n"
}

func yamlString(value string) string {
	return strconv.Quote(value)
}

func posixShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func indentBlock(content string) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	for i, line := range lines {
		if line == "" {
			lines[i] = "      "
			continue
		}
		lines[i] = "      " + line
	}
	return strings.Join(lines, "\n")
}

func readyScript(workRoot string) string {
	return fmt.Sprintf(`#!/bin/sh
set -eu
test -d %s
systemctl is-active --quiet crabbox-vsock-ssh-proxy.service
`, posixShellQuote(workRoot))
}

const vsockProxyService = `[Unit]
Description=Crabbox VSOCK to SSH proxy
After=network.target ssh.service
Wants=ssh.service

[Service]
Type=simple
ExecStart=/usr/local/bin/crabbox-vsock-ssh-proxy.py
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
`

var vsockProxyPython = fmt.Sprintf(`#!/usr/bin/env python3
import socket
import threading
import time

HOST_PORT = %d
POOL_SIZE = %d
ACTIVATE = b"\x01"
READY = b"\x02"
RETRY_SECONDS = 0.25
TARGET = ("127.0.0.1", 22)


def pump(src, dst):
    try:
        while True:
            data = src.recv(32768)
            if not data:
                break
            dst.sendall(data)
    except OSError:
        pass
    finally:
        try:
            dst.shutdown(socket.SHUT_WR)
        except OSError:
            pass


def serve_channel():
    channel = None
    upstream = None
    try:
        channel = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
        channel.connect((socket.VMADDR_CID_HOST, HOST_PORT))
        if channel.recv(1) != ACTIVATE:
            raise ConnectionError("host closed idle channel")
        upstream = socket.create_connection(TARGET)
        channel.sendall(READY)
        t1 = threading.Thread(target=pump, args=(channel, upstream), daemon=True)
        t2 = threading.Thread(target=pump, args=(upstream, channel), daemon=True)
        t1.start()
        t2.start()
        t1.join()
        t2.join()
    except OSError:
        pass
    finally:
        if channel is not None:
            channel.close()
        if upstream is not None:
            upstream.close()


def worker():
    while True:
        serve_channel()
        time.sleep(RETRY_SECONDS)


workers = [threading.Thread(target=worker) for _ in range(POOL_SIZE)]
for worker_thread in workers:
    worker_thread.start()
for worker_thread in workers:
    worker_thread.join()
`, HostVSOCKSSHPort, maxVSOCKProxyChannels)

func validateRuntimeConfig(stateRoot, image, expectedSHA256 string) (map[string]string, error) {
	if _, err := normalizeExpectedSHA256(expectedSHA256); err != nil {
		return nil, err
	}
	_, remote, err := validateRemoteImageURL(image)
	if err != nil {
		return nil, err
	}
	if remote && strings.TrimSpace(expectedSHA256) == "" {
		return nil, fmt.Errorf("apple-vm remote image %q requires a SHA-256 checksum", RedactImageRef(image))
	}
	if !remote {
		if _, err := resolveLocalImagePath(image); err != nil {
			return nil, err
		}
	}
	if _, err := exec.LookPath("hdiutil"); err != nil {
		return nil, fmt.Errorf("hdiutil is required")
	}
	if _, err := exec.LookPath("newfs_msdos"); err != nil {
		return nil, fmt.Errorf("newfs_msdos is required")
	}
	if err := requireHardwareVirtualization(); err != nil {
		return nil, err
	}
	return map[string]string{
		"state_root": stateRoot,
		"image":      ImageIdentity(image, expectedSHA256),
		"runtime":    "virtualization.framework",
		"host":       "darwin/arm64",
	}, nil
}

func requireHardwareVirtualization() error {
	output, err := exec.Command("sysctl", "-n", "kern.hv_support").CombinedOutput()
	if err != nil {
		return fmt.Errorf("check hardware virtualization support: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if strings.TrimSpace(string(output)) != "1" {
		return fmt.Errorf("virtualization is not available on this hardware (kern.hv_support=%s)", strings.TrimSpace(string(output)))
	}
	return nil
}

func attachedDevice(output string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) > 0 && strings.HasPrefix(fields[0], "/dev/disk") {
			return fields[0]
		}
	}
	return ""
}

func copyPlainFile(sourcePath, targetPath string, mode os.FileMode) error {
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	return os.WriteFile(targetPath, data, mode)
}
