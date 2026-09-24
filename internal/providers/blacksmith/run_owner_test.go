//go:build darwin || linux

package blacksmith

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestBlacksmithRunCancellationClosesSyncDescendants(t *testing.T) {
	isolateBlacksmithOwnership(t)
	t.Setenv("CRABBOX_CONTROLLER_PROCESS_TREE_OWNED", "")
	root := t.TempDir()
	t.Setenv("PATH", root+":/usr/bin:/bin")
	writes := filepath.Join(root, "writes")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_ARTIFACT_WRITER_ADDRESS", listener.Addr().String())
	t.Setenv("CRABBOX_ARTIFACT_WRITER_PATH", writes)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A sync child can retain a remote connection without retaining console
	// pipes. Reuse the two-byte writer handshake to expose an early return.
	fakeNative := "#!/bin/sh\nprintf 'Syncing...\\n'\n" + core.ShellQuote(executable) + " -test.run=^TestBlacksmithArtifactWriterHelper$ >/dev/null 2>&1 &\nwait\n"
	if err := os.WriteFile(filepath.Join(root, "blacksmith"), []byte(fakeNative), 0o700); err != nil {
		t.Fatal(err)
	}
	const id = "tbx_sync_owner"
	if _, _, err := core.EnsureTestboxKey(id); err != nil {
		t.Fatal(err)
	}
	backend := newTestBlacksmithBackend(core.BaseConfig(), core.RuntimeForProviderOperation(io.Discard).Exec)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan blacksmithRunOutcome, 1)
	defer cancel()
	go func() {
		done <- backend.runTestbox(ctx, id, []string{"true"}, false, false, nil, nil, nil, nil)
	}()
	child, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if err := child.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var ready [5]byte
	if _, err := io.ReadFull(child, ready[:]); err != nil || string(ready[:]) != "ready" {
		t.Fatalf("sync child readiness=%q err=%v", ready, err)
	}
	cancel()
	select {
	case outcome := <-done:
		if outcome.code == 0 {
			t.Fatal("canceled sync reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled sync did not join")
	}
	// Only release the child's write barrier after Run returns. The old path
	// killed its direct child and allowed this descendant to write afterward.
	_, _ = child.Write([]byte("x"))
	var signal [1]byte
	if _, err := child.Read(signal[:]); !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("sync child connection did not close: %v", err)
	}
	data, err := os.ReadFile(writes)
	if err != nil || string(data) != "x" {
		t.Fatalf("sync descendant wrote after Run returned: data=%q err=%v", data, err)
	}
}
