//go:build linux || darwin

package cli

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAdapterUnixSocketClientAndListenerAreCurrentUserPrivate(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "crabbox-adapter-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "adapter.sock")
	listener, cleanup, err := listenAdapterUnixSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer listener.Close()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 || !adapterFileOwnedByCurrentUser(info) {
		t.Fatalf("socket mode=%s owner_ok=%t", info.Mode(), adapterFileOwnedByCurrentUser(info))
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("path=%q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	client, err := newAdapterLocalClient(path, 150*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if transport, ok := client.Transport.(*http.Transport); !ok || transport.ResponseHeaderTimeout != 150*time.Second {
		t.Fatalf("local adapter response-header timeout=%v", client.Transport)
	}
	response, err := client.Get("http://adapter.local/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
}

func TestAdapterUnixSocketRejectsUnsafePaths(t *testing.T) {
	if _, err := normalizeAdapterUnixSocketPath("relative.sock"); err == nil {
		t.Fatal("relative socket path accepted")
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeAdapterUnixSocketPath(filepath.Join(directory, "adapter.sock")); err == nil {
		t.Fatal("group/world-writable socket directory accepted")
	}
}

func TestAdapterUnixSocketRefusesToReplaceRegularFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "adapter.sock")
	if err := os.WriteFile(path, []byte("do not replace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listenAdapterUnixSocket(path); err == nil {
		t.Fatal("regular file socket path replaced")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "do not replace" {
		t.Fatalf("regular file changed data=%q err=%v", data, err)
	}
}

func TestAdapterUnixSocketRefusesToReplaceLiveListener(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "crabbox-adapter-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "adapter.sock")
	live, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := listenAdapterUnixSocket(path); err == nil {
		t.Fatal("live Unix listener was replaced")
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(original, current) {
		t.Fatalf("live Unix socket changed: err=%v", err)
	}
}

func TestAdapterUnixSocketReplacesDefinitivelyRefusedStaleSocket(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "crabbox-adapter-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "adapter.sock")
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	listener, cleanup, err := listenAdapterUnixSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer listener.Close()
	if conn, err := net.DialTimeout("unix", path, time.Second); err != nil {
		t.Fatal(err)
	} else {
		_ = conn.Close()
	}
}
