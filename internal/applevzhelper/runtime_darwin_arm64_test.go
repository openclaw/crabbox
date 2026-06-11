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
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Code-Hex/vz/v3"
	"golang.org/x/sys/unix"
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

func TestCopyBoundedConsoleLogCapsAndDrainsGuestOutput(t *testing.T) {
	const maxBytes = 128
	source := bytes.NewReader(bytes.Repeat([]byte("guest-output-"), 100))
	var target bytes.Buffer

	copyBoundedConsoleLog(&target, source, maxBytes)

	if source.Len() != 0 {
		t.Fatalf("source has %d unread bytes after log limit", source.Len())
	}
	if target.Len() != maxBytes {
		t.Fatalf("console log size=%d, want %d", target.Len(), maxBytes)
	}
	if !strings.HasSuffix(target.String(), consoleLogTruncatedMarker) {
		t.Fatalf("console log missing truncation marker: %q", target.String())
	}
}

func TestCopyBoundedConsoleLogPreservesShortOutput(t *testing.T) {
	const output = "boot complete\n"
	var target bytes.Buffer

	copyBoundedConsoleLog(&target, strings.NewReader(output), 128)

	if target.String() != output {
		t.Fatalf("console log=%q, want %q", target.String(), output)
	}
}

type failingConsoleLogWriter struct{}

func (failingConsoleLogWriter) Write([]byte) (int, error) {
	return 0, errors.New("disk full")
}

func TestCopyBoundedConsoleLogDrainsAfterWriteFailure(t *testing.T) {
	source := bytes.NewReader(bytes.Repeat([]byte("guest-output-"), 100))

	copyBoundedConsoleLog(failingConsoleLogWriter{}, source, 128)

	if source.Len() != 0 {
		t.Fatalf("source has %d unread bytes after log write failure", source.Len())
	}
}

func TestConsoleLogSinkCapsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	sink, err := newConsoleLogSink(path, 128)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.writeFile.Write(bytes.Repeat([]byte("serial-output-"), 100)); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 128 || !bytes.HasSuffix(data, []byte(consoleLogTruncatedMarker)) {
		t.Fatalf("console log size=%d suffix=%q", len(data), data)
	}
}

func TestServeReverseSSHProxyRecoversAndBoundsActiveChannels(t *testing.T) {
	localListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	guestListener := newQueuedListener()
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveReverseSSHProxyWithTimeouts(localListener, guestListener, 1, 100*time.Millisecond, 100*time.Millisecond)
	}()

	early, err := net.Dial("tcp4", localListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	assertConnectionClosed(t, early)

	staleHost, staleGuest := net.Pipe()
	guestListener.enqueue(staleHost)
	_ = staleGuest.Close()

	hostOne, guestOne := net.Pipe()
	go func() {
		time.Sleep(10 * time.Millisecond)
		guestListener.enqueue(hostOne)
	}()
	activatedOne := make(chan struct{})
	go func() {
		defer guestOne.Close()
		var activation [1]byte
		if _, err := io.ReadFull(guestOne, activation[:]); err == nil && activation[0] == 1 {
			_, _ = guestOne.Write([]byte{2})
			close(activatedOne)
			_, _ = io.Copy(io.Discard, guestOne)
		}
	}()
	clientOne := dialAndWaitForActivation(t, localListener.Addr().String(), activatedOne)

	hostTwo, guestTwo := net.Pipe()
	guestListener.enqueue(hostTwo)
	clientTwo, err := net.Dial("tcp4", localListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	assertConnectionClosed(t, clientTwo)

	_ = clientOne.Close()
	_ = guestOne.Close()
	_ = guestTwo.Close()

	if err := localListener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("serveReverseSSHProxy error=%v, want net.ErrClosed", err)
	}
}

func TestServeReverseSSHProxyReplacesSaturatedStalePool(t *testing.T) {
	localListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	guestListener := newQueuedListener()
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveReverseSSHProxyWithTimeouts(localListener, guestListener, 2, time.Second, 100*time.Millisecond)
	}()

	stalePeers := make([]net.Conn, 0, 2)
	for range 2 {
		host, guest := net.Pipe()
		stalePeers = append(stalePeers, guest)
		guestListener.enqueue(host)
	}
	t.Cleanup(func() {
		for _, peer := range stalePeers {
			_ = peer.Close()
		}
	})

	freshHost, freshGuest := net.Pipe()
	guestListener.enqueue(freshHost)
	activated := make(chan struct{})
	go func() {
		defer freshGuest.Close()
		var activation [1]byte
		if _, err := io.ReadFull(freshGuest, activation[:]); err == nil && activation[0] == 1 {
			_, _ = freshGuest.Write([]byte{2})
			close(activated)
			_, _ = io.Copy(io.Discard, freshGuest)
		}
	}()

	client := dialAndWaitForActivation(t, localListener.Addr().String(), activated)
	_ = client.Close()
	if err := localListener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("serveReverseSSHProxy error=%v, want net.ErrClosed", err)
	}
}

type queuedListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newQueuedListener() *queuedListener {
	return &queuedListener{
		conns:  make(chan net.Conn, 4),
		closed: make(chan struct{}),
	}
}

func (l *queuedListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *queuedListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *queuedListener) Addr() net.Addr {
	return testAddr("vsock")
}

func (l *queuedListener) enqueue(conn net.Conn) {
	l.conns <- conn
}

type testAddr string

func (a testAddr) Network() string { return string(a) }
func (a testAddr) String() string  { return string(a) }

func assertConnectionClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := conn.Read(one[:]); err == nil {
		t.Fatal("connection remained open")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("connection was not closed promptly")
	}
}

func dialAndWaitForActivation(t *testing.T, address string, activated <-chan struct{}) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-activated:
		return conn
	case <-time.After(time.Second):
		_ = conn.Close()
		t.Fatal("guest channel was not activated")
	}
	return nil
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

func TestValidateImageRedirectCannotCrossLoopbackBoundary(t *testing.T) {
	for _, tc := range []struct {
		name           string
		target         string
		originLoopback bool
		wantErr        bool
	}{
		{name: "remote https", target: "https://cdn.example.test/image.img", wantErr: false},
		{name: "remote to loopback https", target: "https://127.0.0.1/image.img", wantErr: true},
		{name: "remote to loopback http", target: "http://localhost/image.img", wantErr: true},
		{name: "loopback http", target: "http://127.0.0.1/image.img", originLoopback: true, wantErr: false},
		{name: "loopback to remote", target: "https://cdn.example.test/image.img", originLoopback: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, err := url.Parse(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			err = validateImageRedirect(target, tc.originLoopback)
			if tc.wantErr && err == nil {
				t.Fatal("redirect accepted")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("redirect rejected: %v", err)
			}
		})
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

func TestValidateRuntimeConfigRejectsMissingLocalImage(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.img")
	_, err := validateRuntimeConfig(t.TempDir(), missing, "")
	if err == nil || !strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("error=%v, want missing local image rejection", err)
	}
}

func TestValidateRuntimeConfigRejectsLocalImageDirectory(t *testing.T) {
	directory := t.TempDir()
	_, err := validateRuntimeConfig(t.TempDir(), directory, "")
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("error=%v, want local image directory rejection", err)
	}
}

func TestValidateRuntimeConfigRejectsLocalImageFIFOWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "image.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := validateRuntimeConfig(t.TempDir(), fifo, "")
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("error=%v, want local image FIFO rejection", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("FIFO validation took %s, want nonblocking rejection", elapsed)
	}
}

func TestResolveSourceImageVerifiesLocalImageWhenChecksumProvided(t *testing.T) {
	stateRoot := t.TempDir()
	path := t.TempDir() + "/image.img"
	payload := []byte("local image")
	sum := sha256.Sum256(payload)
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}

	resolved, err := resolveSourceImage(context.Background(), stateRoot, path, "sha256:"+hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if resolved == path || filepath.Dir(resolved) != DownloadsDir(stateRoot) {
		t.Fatalf("resolved=%q want private cached copy", resolved)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement image"), 0o644); err != nil {
		t.Fatal(err)
	}
	cached, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cached, payload) {
		t.Fatalf("cached image=%q want %q", cached, payload)
	}

	_, err = resolveSourceImage(context.Background(), t.TempDir(), path, strings.Repeat("f", 64))
	if err == nil {
		t.Fatal("expected local checksum mismatch to fail")
	}
}

func TestCacheVerifiedLocalImagePreservesSparseFileAndRejectsOversize(t *testing.T) {
	stateRoot := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), "sparse.raw")
	source, err := os.OpenFile(sourcePath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	const size = int64(32 << 20)
	if err := source.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := source.WriteAt([]byte("sparse-tail"), size-11); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	source, err = os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, source); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	resolved, err := cacheVerifiedLocalImage(context.Background(), stateRoot, sourcePath, hex.EncodeToString(hash.Sum(nil)), size)
	if err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err := unix.Stat(resolved, &stat); err != nil {
		t.Fatal(err)
	}
	if stat.Size != size {
		t.Fatalf("cached size=%d want %d", stat.Size, size)
	}
	if allocated := stat.Blocks * 512; allocated >= size/2 {
		t.Fatalf("cached sparse image allocated=%d logical=%d", allocated, size)
	}

	oversizedPath := filepath.Join(t.TempDir(), "oversized.raw")
	if err := os.WriteFile(oversizedPath, []byte("header"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(oversizedPath, size+1); err != nil {
		t.Fatal(err)
	}
	if _, err := cacheVerifiedLocalImage(context.Background(), t.TempDir(), oversizedPath, strings.Repeat("a", 64), size); err == nil || !strings.Contains(err.Error(), "exceeds configured disk size") {
		t.Fatalf("oversized cache error=%v", err)
	}
}

type recordingSparseTarget struct {
	size   int64
	writes int
	bytes  int
}

func (t *recordingSparseTarget) Truncate(size int64) error {
	t.size = size
	return nil
}

func (t *recordingSparseTarget) WriteAt(data []byte, _ int64) (int, error) {
	t.writes++
	t.bytes += len(data)
	return len(data), nil
}

func TestCopySparseImageWritesScatteredDataAtBlockGranularity(t *testing.T) {
	const size = 8 << 20
	source := make([]byte, size)
	source[0] = 1
	source[4<<20] = 2
	target := &recordingSparseTarget{}

	if _, err := copySparseImageWithSHA256(context.Background(), target, bytes.NewReader(source), size); err != nil {
		t.Fatal(err)
	}
	if target.size != size {
		t.Fatalf("target size=%d want %d", target.size, size)
	}
	if target.bytes > 2*4*1024 {
		t.Fatalf("sparse copy wrote %d bytes across %d writes", target.bytes, target.writes)
	}
}

func TestCacheVerifiedLocalImageChecksHashBeforeParsingQCOW2(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "untrusted.qcow2")
	if err := os.WriteFile(sourcePath, []byte{'Q', 'F', 'I', 0xfb}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(sourcePath, (1<<20)+1); err != nil {
		t.Fatal(err)
	}

	_, err := cacheVerifiedLocalImage(context.Background(), t.TempDir(), sourcePath, strings.Repeat("a", 64), 1<<20)
	if err == nil || !strings.Contains(err.Error(), "does not match expected") {
		t.Fatalf("cache error=%v, want checksum mismatch before QCOW2 parsing", err)
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

func TestCleanupAbandonedCacheTempsPreservesClonedWriters(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.raw")
	if err := os.WriteFile(sourcePath, []byte("source image"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	active, err := createCacheTemp(filepath.Join(dir, "active.raw"))
	if err != nil {
		t.Fatal(err)
	}
	active, _, err = replaceCacheTempWithClone(source, active)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	defer os.Remove(active.Name())

	if err := cleanupAbandonedCacheTemps(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(active.Name()); err != nil {
		t.Fatalf("active cloned cache staging file removed: %v", err)
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

func TestRawImageCacheKeyChangesWhenSourceIdentityChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "image.qcow2")
	fixedTime := time.Unix(1_700_000_000, 0)
	if err := os.WriteFile(path, []byte("first-image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}
	firstInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	first := rawImageCacheKey(path, path, "", firstInfo)

	replacement := filepath.Join(dir, "replacement.qcow2")
	if err := os.WriteFile(replacement, []byte("other-image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if firstInfo.Size() != secondInfo.Size() || !firstInfo.ModTime().Equal(secondInfo.ModTime()) {
		t.Fatal("replacement fixture did not preserve source size and mtime")
	}
	second := rawImageCacheKey(path, path, "", secondInfo)
	if first == second {
		t.Fatal("raw cache key did not change when source file identity changed")
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

func TestValidateQCOW2MetadataBoundsParserAllocations(t *testing.T) {
	image := minimalQCOW2Image()
	if err := validateQCOW2Metadata(bytes.NewReader(image), int64(len(image)), 2<<30); err != nil {
		t.Fatalf("valid qcow2 metadata rejected: %v", err)
	}

	oversizedL1 := append([]byte(nil), image...)
	binary.BigEndian.PutUint32(oversizedL1[36:40], math.MaxUint32)
	if err := validateQCOW2Metadata(bytes.NewReader(oversizedL1), int64(len(oversizedL1)), 2<<30); err == nil || !strings.Contains(err.Error(), "L1 table size") {
		t.Fatalf("oversized L1 validation error=%v", err)
	}

	unalignedL1 := append([]byte(nil), image...)
	binary.BigEndian.PutUint64(unalignedL1[40:48], 65537)
	if err := validateQCOW2Metadata(bytes.NewReader(unalignedL1), int64(len(unalignedL1)), 2<<30); err == nil || !strings.Contains(err.Error(), "not cluster-aligned") {
		t.Fatalf("unaligned L1 validation error=%v", err)
	}

	corrupt := append([]byte(nil), image...)
	binary.BigEndian.PutUint64(corrupt[72:80], 1<<1)
	if err := validateQCOW2Metadata(bytes.NewReader(corrupt), int64(len(corrupt)), 2<<30); err == nil || !strings.Contains(err.Error(), "marked corrupt") {
		t.Fatalf("corrupt image validation error=%v", err)
	}

	largeExtension := append([]byte(nil), image...)
	binary.BigEndian.PutUint32(largeExtension[104:108], 1)
	binary.BigEndian.PutUint32(largeExtension[108:112], 5000)
	if err := validateQCOW2Metadata(bytes.NewReader(largeExtension), int64(len(largeExtension)), 2<<30); err != nil {
		t.Fatalf("valid large extension rejected: %v", err)
	}

	outOfBoundsExtension := append([]byte(nil), image...)
	binary.BigEndian.PutUint32(outOfBoundsExtension[104:108], 1)
	binary.BigEndian.PutUint32(outOfBoundsExtension[108:112], 64<<10)
	if err := validateQCOW2Metadata(bytes.NewReader(outOfBoundsExtension), int64(len(outOfBoundsExtension)), 2<<30); err == nil || !strings.Contains(err.Error(), "exceeds the first cluster") {
		t.Fatalf("out-of-bounds extension validation error=%v", err)
	}
}

func minimalQCOW2Image() []byte {
	const clusterBytes = 64 << 10
	image := make([]byte, 3*clusterBytes)
	copy(image, []byte{'Q', 'F', 'I', 0xfb})
	binary.BigEndian.PutUint32(image[4:8], 3)
	binary.BigEndian.PutUint32(image[20:24], 16)
	binary.BigEndian.PutUint64(image[24:32], 1<<20)
	binary.BigEndian.PutUint32(image[36:40], 1)
	binary.BigEndian.PutUint64(image[40:48], clusterBytes)
	binary.BigEndian.PutUint64(image[48:56], 2*clusterBytes)
	binary.BigEndian.PutUint32(image[56:60], 1)
	binary.BigEndian.PutUint32(image[96:100], 4)
	binary.BigEndian.PutUint32(image[100:104], qcow2HeaderV3Bytes)
	return image
}

func TestEnsureRawImageRejectsBackedSourceBeforeCacheHit(t *testing.T) {
	stateRoot := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), "backed.qcow2")
	image := minimalQCOW2Image()
	binary.BigEndian.PutUint64(image[8:16], qcow2HeaderV3Bytes)
	binary.BigEndian.PutUint32(image[16:20], uint32(len("/tmp/host-secret")))
	if err := os.WriteFile(sourcePath, image, 0o600); err != nil {
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
		`HOST_PORT = 2222`,
		`POOL_SIZE = 32`,
		`channel.connect((socket.VMADDR_CID_HOST, HOST_PORT))`,
		`channel.sendall(READY)`,
	} {
		if !strings.Contains(data, want) {
			t.Fatalf("seed data missing %q:\n%s", want, data)
		}
	}
	if strings.Contains(data, `test -d "/work/$(`) {
		t.Fatalf("readiness script uses expandable double quotes:\n%s", data)
	}
}

func TestCreateSeedImageDetachesWhenCanceledDuringAttach(t *testing.T) {
	original := execSeedImageCommand
	t.Cleanup(func() { execSeedImageCommand = original })
	ctx, cancel := context.WithCancel(context.Background())
	var calls []string
	execSeedImageCommand = func(commandCtx context.Context, name string, args ...string) ([]byte, error) {
		if _, ok := commandCtx.Deadline(); !ok {
			t.Fatalf("%s command has no deadline", name)
		}
		calls = append(calls, strings.Join(append([]string{name}, args...), " "))
		switch {
		case name == "hdiutil" && len(args) > 0 && args[0] == "attach":
			cancel()
			return []byte("/dev/disk99\n"), nil
		case name == "hdiutil" && len(args) > 0 && args[0] == "detach":
			return nil, nil
		default:
			t.Fatalf("unexpected command after attach cancellation: %s %v", name, args)
			return nil, nil
		}
	}

	err := createSeedImage(ctx, filepath.Join(t.TempDir(), "seed.img"), "host", "alice", "ssh-ed25519 AAAATEST", "/work")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("createSeedImage error=%v want context.Canceled", err)
	}
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "hdiutil attach ") || calls[1] != "hdiutil detach /dev/disk99" {
		t.Fatalf("commands=%v", calls)
	}
}

func TestCreateSeedImageDetachesPartialAttachmentOnCommandFailure(t *testing.T) {
	original := execSeedImageCommand
	t.Cleanup(func() { execSeedImageCommand = original })
	var calls []string
	execSeedImageCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(append([]string{name}, args...), " "))
		if name == "hdiutil" && len(args) > 0 && args[0] == "attach" {
			return []byte("hdiutil: operation timed out\n/dev/disk98\n"), context.DeadlineExceeded
		}
		if name == "hdiutil" && len(args) > 0 && args[0] == "detach" {
			return nil, nil
		}
		t.Fatalf("unexpected command: %s %v", name, args)
		return nil, nil
	}

	err := createSeedImage(context.Background(), filepath.Join(t.TempDir(), "seed.img"), "host", "alice", "ssh-ed25519 AAAATEST", "/work")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("createSeedImage error=%v want context.DeadlineExceeded", err)
	}
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "hdiutil attach ") || calls[1] != "hdiutil detach /dev/disk98" {
		t.Fatalf("commands=%v", calls)
	}
}

func TestCreateSeedImageUnmountsAndDetachesWhenCanceledDuringMount(t *testing.T) {
	original := execSeedImageCommand
	t.Cleanup(func() { execSeedImageCommand = original })
	ctx, cancel := context.WithCancel(context.Background())
	var calls []string
	execSeedImageCommand = func(commandCtx context.Context, name string, args ...string) ([]byte, error) {
		if _, ok := commandCtx.Deadline(); !ok {
			t.Fatalf("%s command has no deadline", name)
		}
		calls = append(calls, strings.Join(append([]string{name}, args...), " "))
		switch {
		case name == "hdiutil" && len(args) > 0 && args[0] == "attach":
			return []byte("/dev/disk99\n"), nil
		case name == "newfs_msdos":
			return nil, nil
		case name == "mount":
			cancel()
			return nil, nil
		case name == "umount":
			return nil, nil
		case name == "hdiutil" && len(args) > 0 && args[0] == "detach":
			return nil, nil
		default:
			t.Fatalf("unexpected command: %s %v", name, args)
			return nil, nil
		}
	}

	err := createSeedImage(ctx, filepath.Join(t.TempDir(), "seed.img"), "host", "alice", "ssh-ed25519 AAAATEST", "/work")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("createSeedImage error=%v want context.Canceled", err)
	}
	if len(calls) != 5 ||
		!strings.HasPrefix(calls[0], "hdiutil attach ") ||
		!strings.HasPrefix(calls[1], "newfs_msdos ") ||
		!strings.HasPrefix(calls[2], "mount ") ||
		!strings.HasPrefix(calls[3], "umount ") ||
		calls[4] != "hdiutil detach /dev/disk99" {
		t.Fatalf("commands=%v", calls)
	}
}

func TestCreateSeedImageCleansUpPartialMountOnCommandFailure(t *testing.T) {
	original := execSeedImageCommand
	t.Cleanup(func() { execSeedImageCommand = original })
	var calls []string
	execSeedImageCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(append([]string{name}, args...), " "))
		switch {
		case name == "hdiutil" && len(args) > 0 && args[0] == "attach":
			return []byte("/dev/disk97\n"), nil
		case name == "newfs_msdos":
			return nil, nil
		case name == "mount":
			return []byte("mount timed out"), context.DeadlineExceeded
		case name == "umount":
			return nil, nil
		case name == "hdiutil" && len(args) > 0 && args[0] == "detach":
			return nil, nil
		default:
			t.Fatalf("unexpected command: %s %v", name, args)
			return nil, nil
		}
	}

	err := createSeedImage(context.Background(), filepath.Join(t.TempDir(), "seed.img"), "host", "alice", "ssh-ed25519 AAAATEST", "/work")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("createSeedImage error=%v want context.DeadlineExceeded", err)
	}
	if len(calls) != 5 ||
		!strings.HasPrefix(calls[0], "hdiutil attach ") ||
		!strings.HasPrefix(calls[1], "newfs_msdos ") ||
		!strings.HasPrefix(calls[2], "mount ") ||
		!strings.HasPrefix(calls[3], "umount ") ||
		calls[4] != "hdiutil detach /dev/disk97" {
		t.Fatalf("commands=%v", calls)
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
