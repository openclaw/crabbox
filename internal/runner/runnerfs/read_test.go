package runnerfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func testRoot(t *testing.T) (*Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root, dir
}

func writeFixture(t *testing.T, name, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func symlinkFixture(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func TestReadConfinesAndPreservesPaths(t *testing.T) {
	root, dir := testRoot(t)
	writeFixture(t, filepath.Join(dir, "report.xml"), "INSIDE")
	outside := filepath.Join(t.TempDir(), "outside.xml")
	writeFixture(t, outside, "OUTSIDE")
	symlinkFixture(t, "report.xml", filepath.Join(dir, "relative.xml"))
	symlinkFixture(t, filepath.Join(dir, "report.xml"), filepath.Join(dir, "absolute.xml"))
	symlinkFixture(t, outside, filepath.Join(dir, "outside.xml"))
	for _, name := range []string{"report.xml", "relative.xml", "absolute.xml", filepath.Join(dir, "report.xml")} {
		t.Run(name, func(t *testing.T) {
			file, err := root.Read(name, 64)
			if err != nil || string(file.Data) != "INSIDE" || file.Path != name {
				t.Fatalf("file=%+v err=%v", file, err)
			}
		})
	}
	for _, name := range []string{"outside.xml", outside, "."} {
		file, err := root.Read(name, 64)
		if err == nil || len(file.Data) != 0 {
			t.Fatalf("unsafe read %q: %+v %v", name, file, err)
		}
	}
}

func TestReadLimitNeverReturnsTruncatedData(t *testing.T) {
	root, dir := testRoot(t)
	data := "prefix\n__CRABBOX_RESULT_FILE__:not-a-frame\x00\nsuffix"
	writeFixture(t, filepath.Join(dir, "report"), data)
	file, err := root.Read("report", int64(len(data)))
	if err != nil || !bytes.Equal(file.Data, []byte(data)) {
		t.Fatalf("data changed: %+v %v", file, err)
	}
	file, err = root.Read("report", int64(len(data)-1))
	if !errors.Is(err, ErrLimit) || len(file.Data) != 0 {
		t.Fatalf("partial result: %+v %v", file, err)
	}
}

func TestDistinctReadHonorsCancellationWithoutReturningFileBytes(t *testing.T) {
	root, dir := testRoot(t)
	writeFixture(t, filepath.Join(dir, "report"), "CONTENT")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	file, err := root.readDistinct(ctx, "report", 64, nil)
	if !errors.Is(err, context.Canceled) || len(file.Data) != 0 {
		t.Fatalf("canceled read=%+v err=%v", file, err)
	}
}

func TestReadKeepsAnchoredRootAfterRename(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "root")
	writeFixture(t, filepath.Join(dir, "report"), "ORIGINAL")
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(dir, filepath.Join(parent, "moved")); err != nil {
		t.Skipf("open-directory rename unavailable: %v", err)
	}
	file, err := root.Read("report", 64)
	if err != nil || string(file.Data) != "ORIGINAL" {
		t.Fatalf("lost original root: %+v %v", file, err)
	}
	writeFixture(t, filepath.Join(dir, "report"), "REPLACEMENT")
	file, err = root.Read("report", 64)
	if err != nil || string(file.Data) != "ORIGINAL" {
		t.Fatalf("followed replaced root: %+v %v", file, err)
	}
}

func TestReadParentComponentsKeepRenamedPOSIXRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows parent components require physical path resolution")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "root")
	writeFixture(t, filepath.Join(dir, "sub", "placeholder"), "")
	writeFixture(t, filepath.Join(dir, "report"), "ORIGINAL")
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(dir, filepath.Join(parent, "moved")); err != nil {
		t.Fatal(err)
	}
	file, err := root.Read("sub/../report", 64)
	if err != nil || string(file.Data) != "ORIGINAL" {
		t.Fatalf("lost anchored parent read: data=%q err=%v", file.Data, err)
	}
	writeFixture(t, filepath.Join(dir, "report"), "REPLACEMENT")
	file, err = root.Read("sub/../report", 64)
	if err != nil || string(file.Data) != "ORIGINAL" {
		t.Fatalf("followed replacement: data=%q err=%v", file.Data, err)
	}
}

func TestConcurrentSymlinkReplacementNeverReadsOutside(t *testing.T) {
	root, dir := testRoot(t)
	writeFixture(t, filepath.Join(dir, "inside"), "INSIDE")
	outside := filepath.Join(t.TempDir(), "outside")
	writeFixture(t, outside, "OUTSIDE")
	link := filepath.Join(dir, "link")
	symlinkFixture(t, "inside", link)
	done := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			for _, target := range []string{"inside", outside, filepath.Join(dir, "inside")} {
				tmp := filepath.Join(dir, "next-link")
				_ = os.Remove(tmp)
				if os.Symlink(target, tmp) == nil {
					_ = os.Rename(tmp, link)
				}
			}
		}
	}()
	defer func() { close(done); writers.Wait() }()
	for i := 0; i < 2000; i++ {
		file, err := root.Read("link", 64)
		if err == nil && string(file.Data) != "INSIDE" {
			t.Fatalf("unsafe bytes: %q", file.Data)
		}
	}
}
