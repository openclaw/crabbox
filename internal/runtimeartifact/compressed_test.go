package runtimeartifact

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func compressionFixture(t *testing.T, data []byte) *Artifact {
	t.Helper()
	name := filepath.Join(t.TempDir(), "owned-artifact")
	writeFile(t, name, data)
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	artifact := &Artifact{stream: file, identity: Identity{Target: Target{"linux", "amd64"}, ProtocolVersion: "CBX-REMOTE-1", Size: int64(len(data)), SHA256: digest(data), Capability: Supervisor}}
	t.Cleanup(func() { artifact.Close() })
	return artifact
}

func TestCompressGzipRoundtrip(t *testing.T) {
	data := bytes.Repeat([]byte("owned runtime byte fixture\x00\xff\r\n"), 4096)
	artifact := compressionFixture(t, data)
	if _, err := artifact.Seek(17, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	payload, err := CompressGzip(t.Context(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { payload.Close() })
	if payload.Identity() != artifact.Identity() || payload.Size() >= artifact.Identity().Size {
		t.Fatalf("identity=%+v transport size=%d", payload.Identity(), payload.Size())
	}
	info, err := payload.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		t.Fatalf("temporary payload is not private: %v", info.Mode())
	}
	compressed, err := io.ReadAll(payload)
	if err != nil || int64(len(compressed)) != payload.Size() {
		t.Fatalf("compressed stream length: %d, %v", len(compressed), err)
	}
	if _, err := payload.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	replayed, err := io.ReadAll(payload)
	if err != nil || !bytes.Equal(replayed, compressed) {
		t.Fatalf("replayed compressed stream: %v", err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("gzip roundtrip: %v", err)
	}
	original, err := io.ReadAll(artifact)
	if err != nil || !bytes.Equal(original, data) {
		t.Fatalf("raw fallback stream: %v", err)
	}
	name := payload.file.Name()
	if err := payload.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned temporary file remains: %v", err)
	}
	if err := payload.Close(); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
	if _, err := artifact.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("original artifact was closed: %v", err)
	}
}

func TestCompressGzipCancellationAndCleanup(t *testing.T) {
	data := bytes.Repeat([]byte("cancellation fixture"), 4096)
	artifact := compressionFixture(t, data)
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	marker := filepath.Join(dir, "unrelated-owned-fixture")
	writeFile(t, marker, []byte("keep"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if payload, err := CompressGzip(ctx, artifact); payload != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled compression: payload=%v error=%v", payload, err)
	}
	// A deterministic context cancels when streaming starts, after a temporary
	// file has been created. It uses no timing assumptions or background work.
	streamContext, streamCancel := context.WithCancel(t.Context())
	defer streamCancel()
	ctx = &compressionCancelContext{Context: streamContext, cancel: streamCancel}
	if payload, err := CompressGzip(ctx, artifact); payload != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("streaming cancellation: payload=%v error=%v", payload, err)
	}
	remaining, err := os.ReadDir(dir)
	if err != nil || len(remaining) != 1 || remaining[0].Name() != filepath.Base(marker) {
		t.Fatalf("temporary cleanup changed unrelated files: %v, %v", remaining, err)
	}
	original, err := io.ReadAll(artifact)
	if err != nil || !bytes.Equal(original, data) {
		t.Fatalf("raw fallback after cancellation: %v", err)
	}
}

type compressionCancelContext struct {
	context.Context
	cancel context.CancelFunc
	calls  int
}

func (c *compressionCancelContext) Err() error {
	c.calls++
	if c.calls == 2 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestCompressGzipCloseAfterRemoval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture requires unlinking an open owned temporary file")
	}
	artifact := compressionFixture(t, []byte("owned temporary cleanup fixture"))
	payload, err := CompressGzip(t.Context(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { payload.Close() })
	if err := os.Remove(payload.file.Name()); err != nil {
		t.Fatal(err)
	}
	if err := payload.Close(); err != nil {
		t.Fatalf("already-absent payload: %v", err)
	}
	if err := payload.Close(); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
	if _, err := artifact.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("original artifact was closed: %v", err)
	}
}
