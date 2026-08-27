package runnerfs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestArchivePreservesLiteralPOSIXNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("literal POSIX filenames")
	}
	source := filepath.Join(t.TempDir(), `tree\name`)
	for _, name := range []string{"file..bak", "...", `file\name`} {
		writeFixture(t, filepath.Join(source, name), "content:"+name)
	}
	metadata, archive, err := CreateArchive(t.Context(), source, CreateOptions{}, DefaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	destination, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target, err := ArchiveTarget(destination, metadata.Base, metadata.ContentsOnly)
	if err != nil || target != filepath.Join(destination, `tree\name`) {
		t.Fatalf("target=%q err=%v", target, err)
	}
	stage, err := StageArchive(t.Context(), archive, target, ArchivePayloadRoot, ExtractOptions{}, DefaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	if err := stage.Publish(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file..bak", "...", `file\name`} {
		data, err := os.ReadFile(filepath.Join(target, name))
		if err != nil || string(data) != "content:"+name {
			t.Fatalf("name=%q data=%q err=%v", name, data, err)
		}
	}
}

func TestCopyArchiveRoundTripNormalizesDownloads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX download permission normalization")
	}
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o750); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1_725_000_000, 0)
	file := filepath.Join(source, "executable.sh")
	if err := os.WriteFile(file, []byte("proof\n"), 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(source, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	_, archive, err := CreateArchive(t.Context(), source, CreateOptions{}, DefaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		name := archive.Name()
		_ = archive.Close()
		_ = os.Remove(name)
	})
	stage := t.TempDir()
	if err := ExtractArchive(t.Context(), archive, stage, ArchivePayloadRoot, ExtractOptions{}, DefaultArchiveLimits()); err != nil {
		t.Fatal(err)
	}
	extracted := filepath.Join(stage, ArchivePayloadRoot, "executable.sh")
	data, err := os.ReadFile(extracted)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "proof\n" {
		t.Fatalf("content=%q", data)
	}
	info, err := os.Stat(extracted)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 != 0 {
		t.Fatalf("download mode retained remote executable bits: %o", info.Mode().Perm())
	}
	if !info.ModTime().Equal(mtime) {
		t.Fatalf("mtime=%s, want %s", info.ModTime(), mtime)
	}
}

func TestCreateCopyArchiveSymlinkSafety(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("archive fallback is unavailable on Windows operator hosts")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CreateArchive(t.Context(), link, CreateOptions{}, DefaultArchiveLimits()); err == nil || !strings.Contains(err.Error(), "use -L") {
		t.Fatalf("unsafe symlink err=%v", err)
	}
	_, archive, err := CreateArchive(t.Context(), link, CreateOptions{FollowLinks: true}, DefaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	name := archive.Name()
	_ = archive.Close()
	_ = os.Remove(name)
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	directoryLink := filepath.Join(root, "directory-link")
	if err := os.Symlink(directory, directoryLink); err != nil {
		t.Fatal(err)
	}
	source, archive, err := CreateArchive(t.Context(), directoryLink+string(os.PathSeparator), CreateOptions{FollowLinks: true}, DefaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !source.ContentsOnly {
		t.Fatal("followed directory symlink with trailing slash must copy contents")
	}
	name = archive.Name()
	_ = archive.Close()
	_ = os.Remove(name)
}

func TestCreateCopyArchiveSourcePathIntent(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("archive fallback is unavailable on Windows operator hosts")
	}
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("proof"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CreateArchive(t.Context(), file+"/", CreateOptions{}, DefaultArchiveLimits()); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file trailing slash err=%v", err)
	}
	for _, source := range []string{root + "/.", root + "/.."} {
		if _, _, err := CreateArchive(t.Context(), source, CreateOptions{}, DefaultArchiveLimits()); err == nil || !strings.Contains(err.Error(), "ending in . or ..") {
			t.Fatalf("source=%q err=%v", source, err)
		}
	}
}

func TestExtractValidatedCopyArchiveRejectsUnsafeEntries(t *testing.T) {
	tests := []struct {
		name    string
		header  tar.Header
		body    string
		limits  ArchiveLimits
		wantErr string
	}{
		{
			name:    "path traversal",
			header:  tar.Header{Name: "../escape", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg},
			body:    "x",
			limits:  DefaultArchiveLimits(),
			wantErr: "unsafe path",
		},
		{
			name:    "symlink",
			header:  tar.Header{Name: "remote/link", Linkname: "/tmp/escape", Typeflag: tar.TypeSymlink},
			limits:  DefaultArchiveLimits(),
			wantErr: "unsupported link or special entry",
		},
		{
			name:    "file size",
			header:  tar.Header{Name: "remote/file", Mode: 0o644, Size: 5, Typeflag: tar.TypeReg},
			body:    "12345",
			limits:  ArchiveLimits{MaxEntries: 10, MaxFileBytes: 4, MaxTotalBytes: 10, MaxCompressedBytes: 1024},
			wantErr: "file exceeds size limit",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := testCopyArchive(t, test.header, test.body)
			err := ExtractArchive(t.Context(), bytes.NewReader(archive), t.TempDir(), "remote", ExtractOptions{}, test.limits)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestExtractValidatedCopyArchiveVerifiesChecksum(t *testing.T) {
	archive := testCopyArchive(t, tar.Header{Name: "remote", Mode: 0o644, Size: 5, Typeflag: tar.TypeReg}, "proof")
	archive[len(archive)-5] ^= 0xff
	err := ExtractArchive(t.Context(), bytes.NewReader(archive), t.TempDir(), "remote", ExtractOptions{}, DefaultArchiveLimits())
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err=%v", err)
	}
}

func TestExtractValidatedCopyArchiveBoundsZeroTrailer(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "remote", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, "x"); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(gz, zeroReader{}, 2<<20); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	limits := ArchiveLimits{MaxEntries: 1, MaxFileBytes: 10, MaxTotalBytes: 10, MaxCompressedBytes: 1 << 20}
	err := ExtractArchive(t.Context(), bytes.NewReader(compressed.Bytes()), t.TempDir(), "remote", ExtractOptions{}, limits)
	if err == nil || !strings.Contains(err.Error(), "uncompressed size limit") {
		t.Fatalf("err=%v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(data []byte) (int, error) {
	clear(data)
	return len(data), nil
}

func TestValidatedCopyArchiveEntryPathRejectsWindowsSpecialPaths(t *testing.T) {
	for _, name := range []string{
		`remote\escape`,
		"remote/C:/escape",
		"remote/NUL",
		"remote/COM1.txt",
		"remote/trailing.",
		"remote/trailing ",
	} {
		if _, _, err := validatedCopyArchiveEntryPathForGOOS("windows", name, "remote"); err == nil {
			t.Errorf("Windows archive path %q was accepted", name)
		}
	}
	if _, rel, err := validatedCopyArchiveEntryPathForGOOS("windows", "remote/results/proof.txt", "remote"); err != nil || rel != "results/proof.txt" {
		t.Fatalf("safe Windows path rel=%q err=%v", rel, err)
	}
}

func TestLocalCopyArchiveTargetPreservesTrailingDirectoryIntent(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("archive fallback is unavailable on Windows operator hosts")
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(parent, "new-directory") + string(os.PathSeparator)
	target, err := ArchiveTarget(destination, "source", false)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(parent, "new-directory", "source"); target != want {
		t.Fatalf("target=%q want=%q", target, want)
	}
	if info, err := os.Stat(filepath.Join(parent, "new-directory")); err != nil || !info.IsDir() {
		t.Fatalf("destination directory info=%v err=%v", info, err)
	}
	fileDestination := filepath.Join(parent, "existing-file")
	if err := os.WriteFile(fileDestination, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ArchiveTarget(fileDestination+string(os.PathSeparator), "source", false); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("err=%v", err)
	}
}

func TestRecoverCopyArchiveTransaction(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("archive fallback is unavailable on Windows operator hosts")
	}
	t.Run("restore interrupted backup", func(t *testing.T) {
		parent := t.TempDir()
		target := filepath.Join(parent, "target")
		backup := target + ".crabbox-cp-backup"
		marker := target + ".crabbox-cp-transaction"
		if err := os.WriteFile(backup, []byte("original"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("crabbox-cp-v1\n2147483646\ndead-process\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := recoverCopyArchiveTransaction(target, backup, marker, 0); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(target)
		if err != nil || string(data) != "original" {
			t.Fatalf("content=%q err=%v", data, err)
		}
	})
	t.Run("finish published transaction", func(t *testing.T) {
		parent := t.TempDir()
		target := filepath.Join(parent, "target")
		backup := target + ".crabbox-cp-backup"
		marker := target + ".crabbox-cp-transaction"
		if err := os.WriteFile(target, []byte("new"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(backup, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("crabbox-cp-v1\n2147483646\ndead-process\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := recoverCopyArchiveTransaction(target, backup, marker, 0); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(target)
		if err != nil || string(data) != "new" {
			t.Fatalf("content=%q err=%v", data, err)
		}
		for _, reserved := range []string{backup, marker} {
			if _, err := os.Lstat(reserved); !os.IsNotExist(err) {
				t.Fatalf("reserved path remains: %s err=%v", reserved, err)
			}
		}
	})
	t.Run("reject forged marker", func(t *testing.T) {
		parent := t.TempDir()
		target := filepath.Join(parent, "target")
		backup := target + ".crabbox-cp-backup"
		marker := target + ".crabbox-cp-transaction"
		if err := os.WriteFile(target, []byte("current"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(backup, []byte("reserved"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("crabbox-cp-v1\n2147483646\ndead-process\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := recoverCopyArchiveTransaction(target, backup, marker, 0); err == nil || !strings.Contains(err.Error(), "unauthenticated") {
			t.Fatalf("err=%v", err)
		}
		data, err := os.ReadFile(backup)
		if err != nil || string(data) != "reserved" {
			t.Fatalf("backup=%q err=%v", data, err)
		}
	})
	t.Run("refuse active owner", func(t *testing.T) {
		parent := t.TempDir()
		target := filepath.Join(parent, "target")
		backup := target + ".crabbox-cp-backup"
		marker := target + ".crabbox-cp-transaction"
		identity, ok := copyArchiveProcessIdentity(os.Getpid())
		if !ok {
			t.Fatal("current process identity unavailable")
		}
		if err := os.WriteFile(marker, []byte(fmt.Sprintf("crabbox-cp-v1\n%d\n%s\n", os.Getpid(), identity)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := recoverCopyArchiveTransaction(target, backup, marker, 0); err == nil || !strings.Contains(err.Error(), "another archive copy is active") {
			t.Fatalf("err=%v", err)
		}
		if _, err := os.Lstat(marker); err != nil {
			t.Fatalf("active marker was removed: %v", err)
		}
	})
}

func TestCopyArchiveDirectoryTrackerRejectsFilesystemAliases(t *testing.T) {
	stage := t.TempDir()
	tracker := copyArchiveDirectoryTracker{}
	if err := tracker.ensure(stage, "remote", "A"); err != nil {
		t.Fatal(err)
	}
	upper, err := os.Stat(filepath.Join(stage, ArchivePayloadRoot, "A"))
	if err != nil {
		t.Fatal(err)
	}
	lower, err := os.Stat(filepath.Join(stage, ArchivePayloadRoot, "a"))
	if err != nil || !os.SameFile(upper, lower) {
		t.Skip("test volume is case-sensitive")
	}
	if err := tracker.ensure(stage, "remote", "a"); err == nil || !strings.Contains(err.Error(), "path aliases") {
		t.Fatalf("err=%v", err)
	}
}

func testCopyArchive(t *testing.T, header tar.Header, body string) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&header); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func TestArchivePathSeparator(t *testing.T) {
	if got := hasTrailingPathSeparator(`build\`); got != (runtime.GOOS == "windows") {
		t.Fatalf("native backslash semantics: got=%v", got)
	}
}

func TestCreateArchiveExcludesItsOutputFromSource(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	writeFixture(t, filepath.Join(dir, "source.txt"), "source bytes")
	_, archive, err := CreateArchive(t.Context(), dir, CreateOptions{}, DefaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	gz, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var names []string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
	if len(names) != 2 || names[0] != "payload" || names[1] != "payload/source.txt" {
		t.Fatalf("archive included generated output: %v", names)
	}
}
