//go:build darwin && arm64 && cgo

package applevzhelper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
