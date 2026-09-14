package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func TestManagedStateArchiveRejectsSuppliedMarker(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	writeFile(t, filepath.Join(root, "state", "crabbox", "marker.txt"), "benign state marker\n")
	writeFile(t, filepath.Join(root, "state", "source.txt"), "ordinary source marker\n")
	if archive, err := CreateSyncArchive(context.Background(), Repo{Root: root}, SyncManifest{Files: []string{"state/crabbox/marker.txt"}}, "managed-marker-*.tgz"); err == nil || archive != nil {
		t.Fatal("explicit archive manifest admitted managed marker")
	}
	archive, err := CreateSyncArchive(context.Background(), Repo{Root: root}, SyncManifest{Files: []string{"state/source.txt"}}, "ordinary-marker-*.tgz")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	if names := syncArchiveNames(t, archive); len(names) != 1 || !names["state/source.txt"] {
		t.Fatalf("ordinary archive names=%v", names)
	}
}

func TestCreateSyncArchiveTreatsOptionLikeNamesAsFiles(t *testing.T) {
	root := t.TempDir()
	names := []string{
		"--checkpoint=1",
		"--checkpoint-action=exec=sh pwn.sh",
		"normal.txt",
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	archive, err := CreateSyncArchive(context.Background(), Repo{Root: root}, SyncManifest{Files: names}, "crabbox-sync-test-*.tgz")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()

	got := syncArchiveNames(t, archive)
	for _, name := range names {
		if !got[name] {
			t.Fatalf("archive missing %q; got %#v", name, got)
		}
	}
}

func TestCreateSyncArchiveRejectsUnsafePaths(t *testing.T) {
	_, err := CreateSyncArchive(context.Background(), Repo{Root: t.TempDir()}, SyncManifest{Files: []string{"../secret.txt"}}, "crabbox-sync-test-*.tgz")
	if err == nil {
		t.Fatal("expected unsafe path error")
	}
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 6 {
		t.Fatalf("err=%#v, want sync exit error", err)
	}
}

func TestCopySourceBytesStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &cancelAfterFirstWrite{cancel: cancel}
	written, err := copySourceBytes(ctx, writer, strings.NewReader(strings.Repeat("x", 256*1024)), -1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context canceled", err)
	}
	if writer.writes != 1 || written <= 0 || written >= 256*1024 {
		t.Fatalf("writes=%d bytes=%d", writer.writes, written)
	}
}

func TestCopySourceBytesCancellationOnFinalData(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &cancelAfterFirstWrite{cancel: cancel}
	written, err := copySourceBytes(ctx, writer, iotest.DataErrReader(strings.NewReader("source")), -1)
	if written != 6 || !errors.Is(err, context.Canceled) {
		t.Fatalf("bytes=%d error=%v", written, err)
	}
}

func TestCopySourceBytesLimitsAndCounts(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		limit       int64
		wantLimit   bool
	}{
		{name: "unbounded", input: "source", limit: -1},
		{name: "exact", input: "source", limit: 6},
		{name: "below bound", input: "source", limit: 8},
		{name: "empty", limit: 0},
		{name: "exceeds bound", input: "source", limit: 5, wantLimit: true},
		{name: "exceeds empty bound", input: "source", limit: 0, wantLimit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			written, err := copySourceBytes(context.Background(), &output, strings.NewReader(tc.input), tc.limit)
			if written != int64(output.Len()) {
				t.Fatalf("count=%d actual=%d", written, output.Len())
			}
			if tc.wantLimit {
				if !errors.Is(err, errSourceCopyLimit) || written > tc.limit {
					t.Fatalf("bytes=%d limit=%d error=%v", written, tc.limit, err)
				}
			} else if err != nil || output.String() != tc.input {
				t.Fatalf("output=%q error=%v", output.String(), err)
			}
		})
	}
}

func TestCopySourceBytesReportsShortWrite(t *testing.T) {
	written, err := copySourceBytes(context.Background(), sourceShortWriter{}, strings.NewReader("source"), -1)
	if written != 5 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("bytes=%d error=%v", written, err)
	}
}

type sourceShortWriter struct{}

func (sourceShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func syncArchiveNames(t *testing.T, archive *os.File) map[string]bool {
	t.Helper()
	if _, err := archive.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	got := map[string]bool{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatal(err)
		}
		got[header.Name] = true
	}
}

type cancelAfterFirstWrite struct {
	cancel func()
	writes int
}

func (w *cancelAfterFirstWrite) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == 1 {
		w.cancel()
	}
	return len(p), nil
}
