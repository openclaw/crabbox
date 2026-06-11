//go:build darwin && arm64 && cgo

package applevzhelper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Code-Hex/vz/v3"
)

func TestHandleVMStateErrorPersistsTerminalErrorAndRequestsStop(t *testing.T) {
	stateRoot := t.TempDir()
	name := "vm-error"
	inst := Instance{
		Name:      name,
		Status:    StatusRunning,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := os.MkdirAll(InstanceDir(stateRoot, name), 0o755); err != nil {
		t.Fatalf("create instance dir: %v", err)
	}

	result := handleVMState(vz.VirtualMachineStateError, &inst, stateRoot, name, &bytes.Buffer{})

	if !result.done {
		t.Fatal("expected error state to end serve loop")
	}
	if !result.requestStop {
		t.Fatal("expected error state to request VM stop")
	}
	if result.err == nil || !strings.Contains(result.err.Error(), "vm entered error state") {
		t.Fatalf("unexpected result error: %v", result.err)
	}
	if inst.Status != StatusError || inst.Error != "vm entered VirtualMachineStateError" {
		t.Fatalf("instance status=%q error=%q", inst.Status, inst.Error)
	}
	persisted, err := readMetadata(MetadataPath(stateRoot, name))
	if err != nil {
		t.Fatalf("read persisted metadata: %v", err)
	}
	if persisted.Status != StatusError || persisted.Error != "vm entered VirtualMachineStateError" {
		t.Fatalf("persisted status=%q error=%q", persisted.Status, persisted.Error)
	}
}

func TestResolveSourceImageRequiresChecksumForRemoteImages(t *testing.T) {
	_, err := resolveSourceImage(context.Background(), t.TempDir(), "https://example.test/image.img", "")
	if err == nil {
		t.Fatal("expected remote image without checksum to fail")
	}
	if !strings.Contains(err.Error(), "requires a SHA-256 checksum") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveSourceImageVerifiesRemoteImageChecksum(t *testing.T) {
	payload := []byte("fake image")
	sum := sha256.Sum256(payload)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	path, err := resolveSourceImage(context.Background(), t.TempDir(), server.URL+"/image.img", hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("cached payload=%q", string(got))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("cached image mode=%#o, want 0600", got)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("downloads dir mode=%#o, want 0700", got)
	}

	upperURL := "HTTP" + strings.TrimPrefix(server.URL, "http") + "/upper.img"
	upperPath, err := resolveSourceImage(context.Background(), t.TempDir(), upperURL, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("uppercase HTTP scheme failed: %v", err)
	}
	if _, err := os.Stat(upperPath); err != nil {
		t.Fatalf("uppercase-scheme image missing: %v", err)
	}

	_, err = resolveSourceImage(context.Background(), t.TempDir(), server.URL+"/image.img", strings.Repeat("0", 64))
	if err == nil {
		t.Fatal("expected checksum mismatch to fail")
	}
}

func TestResolveSourceImageRejectsOversizedContentLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "34359738369")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	_, err := resolveSourceImage(context.Background(), t.TempDir(), server.URL+"/image.img", strings.Repeat("a", 64))
	if err == nil || !strings.Contains(err.Error(), "exceeds 32 GiB limit") {
		t.Fatalf("resolveSourceImage error=%v, want size limit", err)
	}
}

func TestCopyRemoteImageRejectsUnknownLengthBeyondLimit(t *testing.T) {
	var target bytes.Buffer
	_, err := copyRemoteImage(&target, strings.NewReader("123456789"), 8)
	if err == nil || !strings.Contains(err.Error(), "exceeds 32 GiB limit") {
		t.Fatalf("copyRemoteImage error=%v, want size limit", err)
	}
	if target.Len() != 9 {
		t.Fatalf("bounded copy wrote %d bytes, want limit plus one", target.Len())
	}
}

func TestResolveSourceImageRejectsNonLoopbackHTTP(t *testing.T) {
	_, err := resolveSourceImage(
		context.Background(),
		t.TempDir(),
		"http://downloads.example.test/bearer-secret/image.img",
		strings.Repeat("a", 64),
	)
	if err == nil {
		t.Fatal("expected non-loopback HTTP image to fail")
	}
	if !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("error=%v, want HTTPS requirement", err)
	}
	if strings.Contains(err.Error(), "bearer-secret") {
		t.Fatalf("error exposes remote image path: %v", err)
	}
}

func TestLoopbackImageHostRejectsProxyableHostnameVariants(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		if !isLoopbackImageHost(host) {
			t.Fatalf("isLoopbackImageHost(%q)=false", host)
		}
	}
	for _, host := range []string{"localhost.", "localhost.example", "downloads.example.test"} {
		if isLoopbackImageHost(host) {
			t.Fatalf("isLoopbackImageHost(%q)=true", host)
		}
	}
}

func TestResolveSourceImageRejectsMalformedRemoteURL(t *testing.T) {
	_, err := resolveSourceImage(
		context.Background(),
		t.TempDir(),
		"https://downloads.example.test/%zz?token=private",
		strings.Repeat("a", 64),
	)
	if err == nil {
		t.Fatal("expected malformed remote URL to fail")
	}
	if !strings.Contains(err.Error(), "invalid URL") {
		t.Fatalf("error=%v, want invalid URL", err)
	}
	if strings.Contains(err.Error(), "token=private") || strings.Contains(err.Error(), "%zz") {
		t.Fatalf("error exposes malformed remote URL: %v", err)
	}
}

func TestResolveSourceImageExcludesURLComponentsFromCacheFilename(t *testing.T) {
	payload := []byte("signed image")
	sum := sha256.Sum256(payload)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	token := strings.Repeat("secret", 100)
	resolved, err := resolveSourceImage(
		context.Background(),
		t.TempDir(),
		server.URL+"/"+token+"/ubuntu.img?token="+token,
		hex.EncodeToString(sum[:]),
	)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(resolved)
	if strings.Contains(name, "token") || strings.Contains(name, "secret") || strings.Contains(name, "ubuntu") {
		t.Fatalf("cache filename exposes URL components: %q", name)
	}
	if !strings.HasSuffix(name, "-image.img") {
		t.Fatalf("cache filename=%q, want fixed remote image name", name)
	}
}

func TestResolveSourceImageKeysRemoteCacheByChecksum(t *testing.T) {
	payload := []byte("first image")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)
	stateRoot := t.TempDir()
	imageURL := server.URL + "/ubuntu.img"

	firstSum := sha256.Sum256(payload)
	firstPath, err := resolveSourceImage(context.Background(), stateRoot, imageURL, hex.EncodeToString(firstSum[:]))
	if err != nil {
		t.Fatal(err)
	}

	payload = []byte("second image")
	secondSum := sha256.Sum256(payload)
	secondPath, err := resolveSourceImage(context.Background(), stateRoot, imageURL, hex.EncodeToString(secondSum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if firstPath == secondPath {
		t.Fatalf("cache paths must differ when checksum changes: %q", firstPath)
	}
	got, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("second cached payload=%q", string(got))
	}
}

func TestResolveSourceImageRedactsRemoteCredentialsFromErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	imageURL := strings.Replace(server.URL, "http://", "http://alice:secret@", 1) + "/ubuntu.img?token=private"

	_, err := resolveSourceImage(context.Background(), t.TempDir(), imageURL, strings.Repeat("a", 64))
	if err == nil {
		t.Fatal("expected remote image error")
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "alice") {
		t.Fatalf("error exposes remote credentials: %v", err)
	}
}

func TestResolveSourceImageRedactsMalformedRedirectFromErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://alice:secret@example.test/%zz?token=private")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)

	_, err := resolveSourceImage(context.Background(), t.TempDir(), server.URL+"/ubuntu.img", strings.Repeat("a", 64))
	if err == nil {
		t.Fatal("expected redirect error")
	}
	for _, secret := range []string{"alice", "secret", "private", "%zz"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error exposes redirect URL component %q: %v", secret, err)
		}
	}
	if !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("error=%v, want credential-free request failure", err)
	}
}

func TestResolveSourceImageDoesNotForwardSignedURLAsRedirectReferer(t *testing.T) {
	payload := []byte("redirected image")
	sum := sha256.Sum256(payload)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if referer := r.Referer(); referer != "" {
			t.Errorf("redirect referer exposes signed URL: %q", referer)
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(target.Close)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/image.img", http.StatusFound)
	}))
	t.Cleanup(source.Close)

	_, err := resolveSourceImage(
		context.Background(),
		t.TempDir(),
		source.URL+"/bearer-secret/image.img?token=private",
		hex.EncodeToString(sum[:]),
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestResolveSourceImageRejectsRedirectToNonLoopbackHTTP(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://downloads.example.test/bearer-secret/image.img", http.StatusFound)
	}))
	t.Cleanup(source.Close)

	_, err := resolveSourceImage(
		context.Background(),
		t.TempDir(),
		source.URL+"/image.img",
		strings.Repeat("a", 64),
	)
	if err == nil {
		t.Fatal("expected HTTP redirect downgrade to fail")
	}
	if !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("error=%v, want credential-free request failure", err)
	}
	if strings.Contains(err.Error(), "bearer-secret") {
		t.Fatalf("error exposes redirect URL: %v", err)
	}
}

func TestResolveSourceImageLimitsRedirects(t *testing.T) {
	var requests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, server.URL+"/loop", http.StatusFound)
	}))
	t.Cleanup(server.Close)

	_, err := resolveSourceImage(context.Background(), t.TempDir(), server.URL+"/loop", strings.Repeat("a", 64))
	if err == nil {
		t.Fatal("expected redirect loop to fail")
	}
	if got := requests.Load(); got > 10 {
		t.Fatalf("redirect loop issued %d requests, want at most 10", got)
	}
	if !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("error=%v, want credential-free request failure", err)
	}
}

func TestValidateRuntimeConfigRejectsNonLoopbackHTTPImage(t *testing.T) {
	_, err := validateRuntimeConfig(
		t.TempDir(),
		"http://downloads.example.test/bearer-secret/image.img",
		strings.Repeat("a", 64),
	)
	if err == nil {
		t.Fatal("expected doctor validation to reject non-loopback HTTP")
	}
	if !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("error=%v, want HTTPS requirement", err)
	}
}

func TestResolveSourceImageVerifiesLocalImageWhenChecksumProvided(t *testing.T) {
	path := t.TempDir() + "/image.img"
	payload := []byte("local image")
	sum := sha256.Sum256(payload)
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	resolved, err := resolveSourceImage(context.Background(), t.TempDir(), path, "sha256:"+hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if resolved != path {
		t.Fatalf("resolved=%q want %q", resolved, path)
	}

	_, err = resolveSourceImage(context.Background(), t.TempDir(), path, strings.Repeat("f", 64))
	if err == nil {
		t.Fatal("expected local checksum mismatch to fail")
	}
}

func TestCreateCacheTempUsesUniqueSiblingFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ubuntu.raw")
	first, err := createCacheTemp(target)
	if err != nil {
		t.Fatal(err)
	}
	firstPath := first.Name()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(firstPath) })

	second, err := createCacheTemp(target)
	if err != nil {
		t.Fatal(err)
	}
	secondPath := second.Name()
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(secondPath) })

	if firstPath == secondPath {
		t.Fatalf("cache staging paths must be unique: %q", firstPath)
	}
	for _, path := range []string{firstPath, secondPath} {
		if filepath.Dir(path) != dir {
			t.Fatalf("staging path %q is not beside target %q", path, target)
		}
		if !strings.HasPrefix(filepath.Base(path), ".ubuntu.raw.tmp-") {
			t.Fatalf("staging path %q does not use target-specific prefix", path)
		}
	}
}

func TestCleanupAbandonedCacheTempsPreservesLockedWriters(t *testing.T) {
	dir := t.TempDir()
	active, err := createCacheTemp(filepath.Join(dir, "active.raw"))
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	defer os.Remove(active.Name())

	abandoned := filepath.Join(dir, ".abandoned.raw.tmp-dead")
	if err := os.WriteFile(abandoned, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupAbandonedCacheTemps(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(active.Name()); err != nil {
		t.Fatalf("active cache staging file removed: %v", err)
	}
	if _, err := os.Stat(abandoned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned cache staging file still exists: %v", err)
	}
}

func TestCacheDirectoryLockSerializesCleanup(t *testing.T) {
	dir := t.TempDir()
	abandoned := filepath.Join(dir, ".abandoned.raw.tmp-dead")
	if err := os.WriteFile(abandoned, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirLock, err := lockCacheDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cleanupAbandonedCacheTemps(dir)
	}()
	select {
	case err := <-done:
		t.Fatalf("cleanup bypassed directory lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := unlockCacheDir(dirLock); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup did not resume after directory unlock")
	}
	if _, err := os.Stat(abandoned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned cache staging file still exists: %v", err)
	}
}

func TestCleanupImageCacheTempsSweepsBothCaches(t *testing.T) {
	stateRoot := t.TempDir()
	for _, dir := range []string{DownloadsDir(stateRoot), ImagesDir(stateRoot)} {
		if err := ensurePrivateDir(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".partial.tmp-dead"), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanupImageCacheTemps(stateRoot); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{DownloadsDir(stateRoot), ImagesDir(stateRoot)} {
		if matches, err := filepath.Glob(filepath.Join(dir, "*.tmp-*")); err != nil {
			t.Fatal(err)
		} else if len(matches) != 0 {
			t.Fatalf("cache %q still contains staging files: %v", dir, matches)
		}
	}
}

func TestRawImageCacheKeyIncludesExpectedChecksum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.qcow2")
	if err := os.WriteFile(path, []byte("same-size-image"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	first := rawImageCacheKey(path, path, strings.Repeat("a", 64), info)
	second := rawImageCacheKey(path, path, strings.Repeat("b", 64), info)
	if first == second {
		t.Fatal("raw cache key did not change with expected checksum")
	}
}

func TestStandaloneQCOW2ReaderRejectsAndPinsBackingFileHeader(t *testing.T) {
	header := make([]byte, 20)
	copy(header, []byte{'Q', 'F', 'I', 0xfb})
	binary.BigEndian.PutUint32(header[4:8], 3)
	reader, err := newStandaloneQCOW2Reader(bytes.NewReader(header))
	if err != nil {
		t.Fatalf("standalone image rejected: %v", err)
	}
	if _, ok := any(reader).(interface{ Name() string }); ok {
		t.Fatal("standalone reader exposes a host filename")
	}

	binary.BigEndian.PutUint64(header[8:16], 4096)
	binary.BigEndian.PutUint32(header[16:20], uint32(len("../../host-secret")))
	var pinned [20]byte
	if _, err := reader.ReadAt(pinned[:], 0); err != nil {
		t.Fatal(err)
	}
	if backingOffset := binary.BigEndian.Uint64(pinned[8:16]); backingOffset != 0 {
		t.Fatalf("pinned backing offset=%d", backingOffset)
	}
	if _, err := newStandaloneQCOW2Reader(bytes.NewReader(header)); err == nil || !strings.Contains(err.Error(), "backing files are not supported") {
		t.Fatalf("backed image validation error=%v", err)
	}
}

func TestEnsureRawImageRejectsBackedSourceBeforeCacheHit(t *testing.T) {
	stateRoot := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), "backed.qcow2")
	header := make([]byte, 20)
	copy(header, []byte{'Q', 'F', 'I', 0xfb})
	binary.BigEndian.PutUint32(header[4:8], 3)
	binary.BigEndian.PutUint64(header[8:16], 4096)
	binary.BigEndian.PutUint32(header[16:20], uint32(len("/tmp/host-secret")))
	if err := os.WriteFile(sourcePath, header, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(ImagesDir(stateRoot)); err != nil {
		t.Fatal(err)
	}
	key := rawImageCacheKey(sourcePath, sourcePath, "", info)
	cachePath := filepath.Join(ImagesDir(stateRoot), hex.EncodeToString(key[:])+".raw")
	if err := os.WriteFile(cachePath, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ensureRawImage(context.Background(), stateRoot, sourcePath, sourcePath, "", 30<<30); err == nil || !strings.Contains(err.Error(), "backing files are not supported") {
		t.Fatalf("ensureRawImage cache-hit error=%v", err)
	}
}

func TestCopyReaderAtRangeRejectsPrematureEOF(t *testing.T) {
	target, err := os.CreateTemp(t.TempDir(), "target-*")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	err = copyReaderAtRange(context.Background(), target, strings.NewReader("short"), 0, 10, make([]byte, 4))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("copyReaderAtRange error=%v, want unexpected EOF", err)
	}
}

func TestCopyReaderAtRangeHonorsCancellation(t *testing.T) {
	target, err := os.CreateTemp(t.TempDir(), "target-*")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = copyReaderAtRange(ctx, target, strings.NewReader("payload"), 0, 7, make([]byte, 4))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copyReaderAtRange error=%v, want context canceled", err)
	}
}

func TestDiskCapacityRejectsOversizedSource(t *testing.T) {
	capacity, err := diskCapacityBytes(30)
	if err != nil {
		t.Fatal(err)
	}
	if capacity != 30<<30 {
		t.Fatalf("capacity=%d", capacity)
	}
	if err := validateSourceDiskSize(capacity, capacity); err != nil {
		t.Fatalf("equal source size rejected: %v", err)
	}
	if err := validateSourceDiskSize(capacity+1, capacity); err == nil {
		t.Fatal("oversized source image accepted")
	}
}

func TestSeedUserDataQuotesYAMLAndReadinessPath(t *testing.T) {
	workRoot := "/work/$(touch /tmp/not-run)'literal"
	data := seedUserData("alice", "ssh-ed25519 AAAATEST alice@example.com", workRoot)
	for _, want := range []string{
		`name: "alice"`,
		`- "ssh-ed25519 AAAATEST alice@example.com"`,
		`[mkdir, -p, "/work/$(touch /tmp/not-run)'literal"]`,
		`test -d '/work/$(touch /tmp/not-run)'\''literal'`,
	} {
		if !strings.Contains(data, want) {
			t.Fatalf("seed data missing %q:\n%s", want, data)
		}
	}
	if strings.Contains(data, `test -d "/work/$(`) {
		t.Fatalf("readiness script uses expandable double quotes:\n%s", data)
	}
}

func TestRequireHardwareVirtualizationRejectsUnsupportedHost(t *testing.T) {
	dir := t.TempDir()
	sysctl := filepath.Join(dir, "sysctl")
	if err := os.WriteFile(sysctl, []byte("#!/bin/sh\nprintf '0\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	err := requireHardwareVirtualization()
	if err == nil || !strings.Contains(err.Error(), "kern.hv_support=0") {
		t.Fatalf("requireHardwareVirtualization error=%v", err)
	}
}
