package runnerfs

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
)

type checkedDirectory struct {
	t       *testing.T
	entries []os.DirEntry
	reads   int
}

func (directory *checkedDirectory) ReadDir(size int) ([]os.DirEntry, error) {
	directory.t.Helper()
	if size != directoryBatchSize {
		directory.t.Fatalf("unbounded directory read: %d", size)
	}
	directory.reads++
	if directory.reads > 3 {
		return nil, io.EOF
	}
	return directory.entries, nil
}

func TestDirectoryEnumerationIsBatchedAndStopsOnError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(root+string(os.PathSeparator)+"file", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	directory := &checkedDirectory{t: t, entries: entries}
	visits := 0
	if err := readDirectory(t.Context(), directory, func(os.DirEntry) error { visits++; return nil }); err != nil || visits != 3 {
		t.Fatalf("visits=%d err=%v", visits, err)
	}
	directory.reads = 0
	stop := errors.New("entry limit reached")
	if err := readDirectory(t.Context(), directory, func(os.DirEntry) error { return stop }); !errors.Is(err, stop) || directory.reads != 1 {
		t.Fatalf("reads=%d err=%v", directory.reads, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	directory.reads = 0
	if err := readDirectory(ctx, directory, func(os.DirEntry) error { return nil }); !errors.Is(err, context.Canceled) || directory.reads != 0 {
		t.Fatalf("canceled enumeration reads=%d err=%v", directory.reads, err)
	}
}
