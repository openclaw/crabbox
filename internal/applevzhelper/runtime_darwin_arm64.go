//go:build darwin && arm64 && cgo

package applevzhelper

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"sync"
	"syscall"
	"time"

	"github.com/Code-Hex/vz/v3"
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
	sourcePath, err := resolveSourceImage(ctx, cfg.StateRoot, cfg.Image, cfg.ImageSHA256)
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
			return "", fmt.Errorf("apple-vz remote image %q requires a SHA-256 checksum", displayImage)
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
		req.Header.Set("User-Agent", "crabbox-apple-vz-helper")
		client := &http.Client{
			Timeout: 2 * time.Hour,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				req.Header.Del("Referer")
				if len(via) >= 10 {
					return fmt.Errorf("stopped after 10 redirects")
				}
				req.URL.Scheme = strings.ToLower(req.URL.Scheme)
				if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
					return fmt.Errorf("unsupported redirect scheme")
				}
				if req.URL.Scheme == "http" && !isLoopbackImageHost(req.URL.Hostname()) {
					return fmt.Errorf("redirect to non-loopback HTTP is not allowed")
				}
				return nil
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
	path := image
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
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("image %q: %w", path, err)
	}
	if checksum != "" {
		if err := verifyFileSHA256(path, checksum); err != nil {
			return "", err
		}
	}
	return path, nil
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
		return nil, true, fmt.Errorf("apple-vz remote images must use HTTPS; HTTP is allowed only for loopback development")
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
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open image for sha256: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fmt.Errorf("hash image: %w", err)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expected {
		return fmt.Errorf("verify image %q: sha256 %s does not match expected %s", path, actual, expected)
	}
	return nil
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

func rawImageCacheKey(sourceRef, sourcePath, checksum string, info os.FileInfo) [sha256.Size]byte {
	return sha256.Sum256([]byte(fmt.Sprintf(
		"%s|%s|%s|%d|%d",
		sourceRef,
		sourcePath,
		checksum,
		info.Size(),
		info.ModTime().UnixNano(),
	)))
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
	header := make([]byte, 4)
	if _, err := io.ReadFull(file, header); err != nil {
		return false, fmt.Errorf("read image header: %w", err)
	}
	return bytes.Equal(header, []byte{'Q', 'F', 'I', 0xfb}), nil
}

func convertQCOW2ToRaw(ctx context.Context, sourcePath string, target *os.File, maxDiskBytes int64) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open qcow2 image: %w", err)
	}
	defer source.Close()
	image, err := qcow2reader.Open(source)
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
	defer target.Close()
	if err := target.Truncate(info.Size()); err != nil {
		return fmt.Errorf("size target file: %w", err)
	}
	buf := make([]byte, 4*1024*1024)
	return copyReaderAtRange(ctx, target, source, 0, info.Size(), buf)
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
		if !allZero(part) {
			if _, err := target.WriteAt(part, offset+copied); err != nil {
				return err
			}
		}
		copied += int64(n)
		if errors.Is(err, io.EOF) && copied < length {
			return io.ErrUnexpectedEOF
		}
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

func createSeedImage(ctx context.Context, path, hostName, user, publicKey, workRoot string) error {
	tmpDir, err := os.MkdirTemp("", "crabbox-apple-vz-seed-*")
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
		file.Close()
		return fmt.Errorf("size seed image: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("create seed image: %w", err)
	}
	// Register the attached device before honoring cancellation so cleanup can
	// always detach it.
	attachOut, err := exec.Command("hdiutil", "attach", "-imagekey", "diskimage-class=CRawDiskImage", "-nomount", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("attach seed image: %w: %s", err, strings.TrimSpace(string(attachOut)))
	}
	device := firstFieldLine(string(attachOut))
	if device == "" {
		return fmt.Errorf("attach seed image: missing device name")
	}
	detached := false
	defer func() {
		if !detached {
			_, _ = exec.Command("hdiutil", "detach", device).CombinedOutput()
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "newfs_msdos", "-F", "16", "-v", "cidata", device).CombinedOutput(); err != nil {
		return fmt.Errorf("format seed image: %w: %s", err, strings.TrimSpace(string(out)))
	}
	mountDir := filepath.Join(tmpDir, "mnt")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		return fmt.Errorf("create seed mount dir: %w", err)
	}
	// As with attachment, establish the mounted state before honoring
	// cancellation so the deferred unmount cannot be skipped.
	if out, err := exec.Command("mount", "-t", "msdos", device, mountDir).CombinedOutput(); err != nil {
		return fmt.Errorf("mount seed image: %w: %s", err, strings.TrimSpace(string(out)))
	}
	mounted := true
	defer func() {
		if mounted {
			_, _ = exec.Command("umount", mountDir).CombinedOutput()
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, name := range []string{"meta-data", "user-data"} {
		if err := copyPlainFile(filepath.Join(tmpDir, name), filepath.Join(mountDir, name), 0o644); err != nil {
			return fmt.Errorf("populate seed image: %w", err)
		}
	}
	if out, err := exec.CommandContext(ctx, "umount", mountDir).CombinedOutput(); err != nil {
		return fmt.Errorf("unmount seed image: %w: %s", err, strings.TrimSpace(string(out)))
	}
	mounted = false
	if out, err := exec.CommandContext(ctx, "hdiutil", "detach", device).CombinedOutput(); err != nil {
		return fmt.Errorf("detach seed image: %w: %s", err, strings.TrimSpace(string(out)))
	}
	detached = true
	return nil
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

const vsockProxyPython = `#!/usr/bin/env python3
import socket
import threading

LISTEN_PORT = 2222
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


def handle(conn):
    upstream = socket.create_connection(TARGET)
    t1 = threading.Thread(target=pump, args=(conn, upstream), daemon=True)
    t2 = threading.Thread(target=pump, args=(upstream, conn), daemon=True)
    t1.start()
    t2.start()
    t1.join()
    t2.join()
    try:
        conn.close()
    finally:
        upstream.close()


sock = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind((socket.VMADDR_CID_ANY, LISTEN_PORT))
sock.listen(16)

while True:
    conn, _ = sock.accept()
    threading.Thread(target=handle, args=(conn,), daemon=True).start()
`

func runServe(stateRoot, name string, stdout, stderr io.Writer) error {
	inst, err := initializeServeInstance(stateRoot, name)
	if err != nil {
		return err
	}
	vmConfig, closers, err := buildVMConfig(inst)
	if err != nil {
		inst.Status = StatusError
		inst.Error = err.Error()
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), inst)
		return err
	}
	defer closeFiles(closers)
	if ok, err := vmConfig.Validate(); err != nil {
		inst.Status = StatusError
		inst.Error = err.Error()
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), inst)
		return fmt.Errorf("validate vm config: %w", err)
	} else if !ok {
		inst.Status = StatusError
		inst.Error = "invalid vm configuration"
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), inst)
		return fmt.Errorf("invalid vm configuration")
	}
	vm, err := vz.NewVirtualMachine(vmConfig)
	if err != nil {
		inst.Status = StatusError
		inst.Error = err.Error()
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), inst)
		return fmt.Errorf("create virtual machine: %w", err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		inst.Status = StatusError
		inst.Error = err.Error()
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), inst)
		return fmt.Errorf("open local ssh proxy: %w", err)
	}
	defer listener.Close()
	tcpAddr, _ := listener.Addr().(*net.TCPAddr)
	inst.SSHHost = "127.0.0.1"
	inst.SSHPort = tcpAddr.Port
	inst.UpdatedAt = time.Now().UTC()
	if err := writeMetadata(MetadataPath(stateRoot, name), inst); err != nil {
		return err
	}
	proxyErr := make(chan error, 1)
	go func() {
		proxyErr <- serveLocalSSHProxy(listener, vm)
	}()
	if err := vm.Start(); err != nil {
		inst.Status = StatusError
		inst.Error = err.Error()
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), inst)
		return fmt.Errorf("start virtual machine: %w", err)
	}
	inst.Status = StatusRunning
	inst.Error = ""
	inst.UpdatedAt = time.Now().UTC()
	if err := writeMetadata(MetadataPath(stateRoot, name), inst); err != nil {
		return err
	}
	sigCh := make(chan os.Signal, 2)
	signalNotify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signalStop(sigCh)
	stateCh := vm.StateChangedNotify()
	for {
		select {
		case err := <-proxyErr:
			if err == nil || errors.Is(err, net.ErrClosed) {
				continue
			}
			inst.Status = StatusError
			inst.Error = err.Error()
			inst.UpdatedAt = time.Now().UTC()
			_ = writeMetadata(MetadataPath(stateRoot, name), inst)
			if stopErr := requestStop(vm); stopErr != nil {
				fmt.Fprintf(stderr, "apple-vz helper stop request failed for %s after proxy error: %v\n", name, stopErr)
			}
			return err
		case sig := <-sigCh:
			fmt.Fprintf(stderr, "apple-vz helper received %s for %s\n", sig.String(), name)
			inst.Status = StatusStopping
			inst.UpdatedAt = time.Now().UTC()
			_ = writeMetadata(MetadataPath(stateRoot, name), inst)
			if err := requestStop(vm); err != nil {
				fmt.Fprintf(stderr, "apple-vz helper stop request failed for %s: %v\n", name, err)
			}
		case state := <-stateCh:
			result := handleVMState(state, &inst, stateRoot, name, stdout)
			if result.requestStop {
				if err := requestStop(vm); err != nil {
					fmt.Fprintf(stderr, "apple-vz helper stop request failed for %s: %v\n", name, err)
				}
			}
			if result.done {
				return result.err
			}
		}
	}
}

func initializeServeInstance(stateRoot, name string) (Instance, error) {
	inst, err := readMetadata(MetadataPath(stateRoot, name))
	if err != nil {
		return Instance{}, err
	}
	inst.PID = os.Getpid()
	startedAt, err := processStartTime(inst.PID)
	if err != nil {
		return Instance{}, fmt.Errorf("read helper process identity: %w", err)
	}
	inst.PIDStartedAt = startedAt
	inst.Status = StatusStarting
	inst.UpdatedAt = time.Now().UTC()
	if err := writeMetadataFunc(MetadataPath(stateRoot, name), inst); err != nil {
		return Instance{}, err
	}
	return inst, nil
}

type vmStateResult struct {
	done        bool
	requestStop bool
	err         error
}

func handleVMState(state vz.VirtualMachineState, inst *Instance, stateRoot, name string, stdout io.Writer) vmStateResult {
	switch state {
	case vz.VirtualMachineStateRunning:
		inst.Status = StatusRunning
		inst.Error = ""
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), *inst)
	case vz.VirtualMachineStateStopping:
		inst.Status = StatusStopping
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), *inst)
	case vz.VirtualMachineStateStopped:
		inst.Status = StatusStopped
		inst.Error = ""
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), *inst)
		return vmStateResult{done: true}
	case vz.VirtualMachineStateError:
		inst.Status = StatusError
		inst.Error = "vm entered VirtualMachineStateError"
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), *inst)
		return vmStateResult{
			done:        true,
			requestStop: true,
			err:         fmt.Errorf("vm entered error state"),
		}
	default:
		fmt.Fprintf(stdout, "apple-vz helper state=%s name=%s\n", state.String(), name)
	}
	return vmStateResult{}
}

func buildVMConfig(inst Instance) (*vz.VirtualMachineConfiguration, []*os.File, error) {
	var closers []*os.File
	efiStore, err := vz.NewEFIVariableStore(inst.EFIVariableStorePath, vz.WithCreatingEFIVariableStore())
	if err != nil {
		return nil, closers, fmt.Errorf("create EFI variable store: %w", err)
	}
	if err := os.Chmod(inst.EFIVariableStorePath, 0o600); err != nil {
		return nil, closers, fmt.Errorf("secure EFI variable store: %w", err)
	}
	bootLoader, err := vz.NewEFIBootLoader(vz.WithEFIVariableStore(efiStore))
	if err != nil {
		return nil, closers, fmt.Errorf("create EFI boot loader: %w", err)
	}
	config, err := vz.NewVirtualMachineConfiguration(bootLoader, uint(inst.CPUs), uint64(inst.MemoryMiB)*1024*1024)
	if err != nil {
		return nil, closers, fmt.Errorf("create vm config: %w", err)
	}
	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, closers, fmt.Errorf("create entropy device: %w", err)
	}
	config.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})
	nat, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, closers, fmt.Errorf("create nat attachment: %w", err)
	}
	netDevice, err := vz.NewVirtioNetworkDeviceConfiguration(nat)
	if err != nil {
		return nil, closers, fmt.Errorf("create network device: %w", err)
	}
	config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netDevice})
	socketDevice, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, closers, fmt.Errorf("create socket device: %w", err)
	}
	config.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{socketDevice})
	rootAttachment, err := vz.NewDiskImageStorageDeviceAttachment(inst.DiskPath, false)
	if err != nil {
		return nil, closers, fmt.Errorf("open root disk: %w", err)
	}
	rootDevice, err := vz.NewVirtioBlockDeviceConfiguration(rootAttachment)
	if err != nil {
		return nil, closers, fmt.Errorf("configure root disk: %w", err)
	}
	seedAttachment, err := vz.NewDiskImageStorageDeviceAttachment(inst.SeedPath, true)
	if err != nil {
		return nil, closers, fmt.Errorf("open seed disk: %w", err)
	}
	seedDevice, err := vz.NewVirtioBlockDeviceConfiguration(seedAttachment)
	if err != nil {
		return nil, closers, fmt.Errorf("configure seed disk: %w", err)
	}
	config.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{rootDevice, seedDevice})
	if inst.ConsoleLogPath != "" {
		readFile, err := os.Open("/dev/null")
		if err != nil {
			return nil, closers, fmt.Errorf("open /dev/null: %w", err)
		}
		writeFile, err := os.OpenFile(inst.ConsoleLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			readFile.Close()
			return nil, closers, fmt.Errorf("open console log: %w", err)
		}
		if err := writeFile.Chmod(0o600); err != nil {
			readFile.Close()
			writeFile.Close()
			return nil, closers, fmt.Errorf("secure console log: %w", err)
		}
		closers = append(closers, readFile, writeFile)
		consoleAttachment, err := vz.NewFileHandleSerialPortAttachment(readFile, writeFile)
		if err != nil {
			return nil, closers, fmt.Errorf("create serial console: %w", err)
		}
		consolePort, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(consoleAttachment)
		if err != nil {
			return nil, closers, fmt.Errorf("configure serial console: %w", err)
		}
		config.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{consolePort})
	}
	return config, closers, nil
}

func closeFiles(files []*os.File) {
	for _, file := range files {
		if file != nil {
			_ = file.Close()
		}
	}
}

func serveLocalSSHProxy(listener net.Listener, vm *vz.VirtualMachine) error {
	socketDevices := vm.SocketDevices()
	if len(socketDevices) == 0 {
		return fmt.Errorf("vm socket device unavailable")
	}
	socketDevice := socketDevices[0]
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		wg.Add(1)
		go func(localConn net.Conn) {
			defer wg.Done()
			defer localConn.Close()
			guestConn, err := socketDevice.Connect(GuestVSOCKSSHPort)
			if err != nil {
				return
			}
			defer guestConn.Close()
			bridgeConnections(localConn, guestConn)
		}(conn)
	}
}

func bridgeConnections(left, right net.Conn) {
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		_ = dst.SetDeadline(time.Now())
		done <- struct{}{}
	}
	go pipe(left, right)
	go pipe(right, left)
	<-done
	<-done
}

func requestStop(vm *vz.VirtualMachine) error {
	if vm.CanRequestStop() {
		ok, err := vm.RequestStop()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	if vm.CanStop() {
		return vm.Stop()
	}
	return nil
}

func validateRuntimeConfig(stateRoot, image, expectedSHA256 string) (map[string]string, error) {
	if _, err := normalizeExpectedSHA256(expectedSHA256); err != nil {
		return nil, err
	}
	_, remote, err := validateRemoteImageURL(image)
	if err != nil {
		return nil, err
	}
	if remote && strings.TrimSpace(expectedSHA256) == "" {
		return nil, fmt.Errorf("apple-vz remote image %q requires a SHA-256 checksum", RedactImageRef(image))
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
	tmpDir, err := os.MkdirTemp("", "crabbox-apple-vz-doctor-*")
	if err != nil {
		return nil, fmt.Errorf("create doctor temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	rootDisk := filepath.Join(tmpDir, "disk.raw")
	seedDisk := filepath.Join(tmpDir, "seed.raw")
	efiPath := filepath.Join(tmpDir, "efi.bin")
	if err := vz.CreateDiskImage(rootDisk, 64*1024*1024); err != nil {
		return nil, fmt.Errorf("create doctor root disk: %w", err)
	}
	if err := vz.CreateDiskImage(seedDisk, 8*1024*1024); err != nil {
		return nil, fmt.Errorf("create doctor seed disk: %w", err)
	}
	inst := Instance{
		Name:                 "doctor",
		Image:                ImageIdentity(image, expectedSHA256),
		SSHUser:              "crabbox",
		WorkRoot:             "/work/crabbox",
		CPUs:                 2,
		MemoryMiB:            2048,
		DiskPath:             rootDisk,
		SeedPath:             seedDisk,
		EFIVariableStorePath: efiPath,
		ConsoleLogPath:       filepath.Join(tmpDir, "console.log"),
	}
	config, closers, err := buildVMConfig(inst)
	if err != nil {
		return nil, err
	}
	defer closeFiles(closers)
	if ok, err := config.Validate(); err != nil {
		return nil, fmt.Errorf("validate runtime config: %w", err)
	} else if !ok {
		return nil, fmt.Errorf("validate runtime config: invalid configuration")
	}
	if _, err := vz.NewVirtualMachine(config); err != nil {
		return nil, fmt.Errorf("create runtime VM: %w", err)
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

func firstFieldLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) > 0 {
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
