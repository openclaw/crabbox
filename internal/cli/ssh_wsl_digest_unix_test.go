//go:build !windows

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type wslDigestDeadlineReader struct {
	*bytes.Reader
	wait  func()
	reads int
	seeks int
}

func (r *wslDigestDeadlineReader) Seek(offset int64, whence int) (int64, error) {
	r.seeks++
	return r.Reader.Seek(offset, whence)
}

func TestWSLStagePrefixDigestPreservesCancellationCause(t *testing.T) {
	for _, test := range []struct {
		name string
		size int
		pre  bool
	}{
		{"before reset", 1, true},
		{"during final read", 1, false},
		{"between chunks", 64 << 10, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cause := errors.New("cleanup hashing stopped")
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(cause)
			reader := &wslDigestDeadlineReader{
				Reader: bytes.NewReader(bytes.Repeat([]byte("x"), test.size)),
				wait:   func() { cancel(cause) },
			}
			spool := &wslStageSpool{size: int64(test.size), input: &replayableSSHInput{reader: reader}}
			wantReads := 1
			if test.pre {
				cancel(cause)
				wantReads = 0
			}
			digest, err := spool.prefixDigest(ctx, int64(test.size))
			if !errors.Is(err, cause) || digest != [sha256.Size]byte{} || reader.reads != wantReads || reader.seeks != wantReads {
				t.Fatalf("hash=%x error=%v reads=%d seeks=%d; want cancellation without a usable digest", digest, err, reader.reads, reader.seeks)
			}
		})
	}
}

func (r *wslDigestDeadlineReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 1 {
		r.wait()
	}
	return r.Reader.Read(p)
}

func TestWSLStagePartialCleanupStopsHashingWhenBudgetEnds(t *testing.T) {
	root := t.TempDir()
	spool, data := newTestWSLStageSpool(t, bytes.Repeat([]byte("x"), 128<<10))
	nonce := strings.Repeat("e", 32)
	directory := filepath.Join(root, filepath.FromSlash(wslStageRoot))
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(directory, nonce+".part")
	if err := os.WriteFile(part, data, 0600); err != nil {
		t.Fatal(err)
	}
	oldStart, oldDiscard := startWSLStageCleanupSubsystem, discardWSLStageFile
	t.Cleanup(func() {
		startWSLStageCleanupSubsystem, discardWSLStageFile = oldStart, oldDiscard
	})
	var cleanupCtx context.Context
	startWSLStageCleanupSubsystem = func(ctx context.Context, _ SSHTarget, _, _, _ string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
		cleanupCtx = ctx
		return startLoopbackWSLSFTPSubsystem(ctx, root, func(conn net.Conn) net.Conn { return conn })
	}
	discards := 0
	discardWSLStageFile = func(ctx context.Context, _ SSHTarget, _, _ string, _ int64, _ [sha256.Size]byte, _ string) error {
		discards++
		return context.Cause(ctx)
	}
	reader := &wslDigestDeadlineReader{Reader: bytes.NewReader(data), wait: func() { <-cleanupCtx.Done() }}
	spool.input = &replayableSSHInput{reader: reader}
	err := cleanupWSLStagePart(t.Context(), SSHTarget{}, nonce, spool, 250*time.Millisecond, 250*time.Millisecond, "1")
	if !errors.Is(err, context.DeadlineExceeded) || reader.reads != 1 || discards != 0 {
		t.Fatalf("cleanup=%v reads=%d discards=%d; want deadline after one read without deletion", err, reader.reads, discards)
	}
	got, err := os.ReadFile(part)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("unverified partial changed after hashing deadline: %v", err)
	}
}
