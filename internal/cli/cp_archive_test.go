package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCopyArchiveRoundTripNormalizesDownloads(t *testing.T) {
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

	_, archive, err := createCopyArchive(t.Context(), source, false, defaultCopyArchiveLimits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		name := archive.Name()
		_ = archive.Close()
		_ = os.Remove(name)
	})
	stage := t.TempDir()
	if err := extractValidatedCopyArchive(t.Context(), archive, stage, copyArchivePayloadRoot, defaultCopyArchiveLimits); err != nil {
		t.Fatal(err)
	}
	extracted := filepath.Join(stage, copyArchivePayloadRoot, "executable.sh")
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
	if _, _, err := createCopyArchive(t.Context(), link, false, defaultCopyArchiveLimits); err == nil || !strings.Contains(err.Error(), "use -L") {
		t.Fatalf("unsafe symlink err=%v", err)
	}
	_, archive, err := createCopyArchive(t.Context(), link, true, defaultCopyArchiveLimits)
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
	source, archive, err := createCopyArchive(t.Context(), directoryLink+string(os.PathSeparator), true, defaultCopyArchiveLimits)
	if err != nil {
		t.Fatal(err)
	}
	if !source.contentsOnly {
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
	if _, _, err := createCopyArchive(t.Context(), file+"/", false, defaultCopyArchiveLimits); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file trailing slash err=%v", err)
	}
	for _, source := range []string{root + "/.", root + "/.."} {
		if _, _, err := createCopyArchive(t.Context(), source, false, defaultCopyArchiveLimits); err == nil || !strings.Contains(err.Error(), "ending in . or ..") {
			t.Fatalf("source=%q err=%v", source, err)
		}
	}
}

func TestExtractValidatedCopyArchiveRejectsUnsafeEntries(t *testing.T) {
	tests := []struct {
		name    string
		header  tar.Header
		body    string
		limits  copyArchiveLimits
		wantErr string
	}{
		{
			name:    "path traversal",
			header:  tar.Header{Name: "../escape", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg},
			body:    "x",
			limits:  defaultCopyArchiveLimits,
			wantErr: "unsafe path",
		},
		{
			name:    "symlink",
			header:  tar.Header{Name: "remote/link", Linkname: "/tmp/escape", Typeflag: tar.TypeSymlink},
			limits:  defaultCopyArchiveLimits,
			wantErr: "unsupported link or special entry",
		},
		{
			name:    "file size",
			header:  tar.Header{Name: "remote/file", Mode: 0o644, Size: 5, Typeflag: tar.TypeReg},
			body:    "12345",
			limits:  copyArchiveLimits{maxEntries: 10, maxFileBytes: 4, maxTotalBytes: 10, maxCompressedBytes: 1024},
			wantErr: "file exceeds size limit",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := testCopyArchive(t, test.header, test.body)
			err := extractValidatedCopyArchive(t.Context(), bytes.NewReader(archive), t.TempDir(), "remote", test.limits)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestExtractValidatedCopyArchiveVerifiesChecksum(t *testing.T) {
	archive := testCopyArchive(t, tar.Header{Name: "remote", Mode: 0o644, Size: 5, Typeflag: tar.TypeReg}, "proof")
	archive[len(archive)-5] ^= 0xff
	err := extractValidatedCopyArchive(t.Context(), bytes.NewReader(archive), t.TempDir(), "remote", defaultCopyArchiveLimits)
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
	limits := copyArchiveLimits{maxEntries: 1, maxFileBytes: 10, maxTotalBytes: 10, maxCompressedBytes: 1 << 20}
	err := extractValidatedCopyArchive(t.Context(), bytes.NewReader(compressed.Bytes()), t.TempDir(), "remote", limits)
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

func TestCopyOverResolvedSSHArchiveFallbackWithOldRsync(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX fake SSH helper")
	}
	tools := t.TempDir()
	sshArgs := filepath.Join(tools, "ssh-args")
	rsyncTransfer := filepath.Join(tools, "rsync-transfer")
	secret := "opaque-user-value"
	writeExecutable(t, filepath.Join(tools, "rsync"), `#!/bin/sh
set -eu
if [ "${1:-}" = "--version" ]; then
  printf 'openrsync: protocol version 29\nrsync version 2.6.9 compatible\n'
  exit 0
fi
printf started > "$CRABBOX_TEST_RSYNC_TRANSFER"
exit 91
`)
	writeExecutable(t, filepath.Join(tools, "ssh"), `#!/bin/sh
set -eu
printf '%s\n' "$@" >> "$CRABBOX_TEST_SSH_ARGS"
printf '%s: transport diagnostic\n' "$CRABBOX_TEST_SECRET" >&2
for last do :; done
exec /bin/bash -c "$last"
`)
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRABBOX_TEST_SSH_ARGS", sshArgs)
	t.Setenv("CRABBOX_TEST_RSYNC_TRANSFER", rsyncTransfer)
	t.Setenv("CRABBOX_TEST_SECRET", secret)

	sourceParent := t.TempDir()
	source := filepath.Join(sourceParent, "input")
	if err := os.Mkdir(source, 0o750); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1_725_000_123, 0)
	if err := os.WriteFile(filepath.Join(source, "proof.txt"), []byte("archive-proof"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(source, "proof.txt"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	remoteDestination := t.TempDir()
	target := SSHTarget{User: secret, Host: "example.test", Port: "22", AuthSecret: true}
	var stderr bytes.Buffer
	if err := copyOverResolvedSSH(t.Context(), target, source, "SANDBOX:"+remoteDestination, false, io.Discard, &stderr); err != nil {
		t.Fatal(err)
	}
	remoteFile := filepath.Join(remoteDestination, filepath.Base(source), "proof.txt")
	data, err := os.ReadFile(remoteFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "archive-proof" {
		t.Fatalf("remote content=%q", data)
	}
	remoteInfo, err := os.Stat(remoteFile)
	if err != nil {
		t.Fatal(err)
	}
	if remoteInfo.Mode().Perm() != 0o640 || !remoteInfo.ModTime().Equal(mtime) {
		t.Fatalf("remote metadata mode=%o mtime=%s", remoteInfo.Mode().Perm(), remoteInfo.ModTime())
	}

	downloadParent := t.TempDir()
	if err := copyOverResolvedSSH(t.Context(), target, "SANDBOX:"+remoteFile, downloadParent, false, io.Discard, &stderr); err != nil {
		t.Fatal(err)
	}
	downloaded := filepath.Join(downloadParent, "proof.txt")
	data, err = os.ReadFile(downloaded)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "archive-proof" {
		t.Fatalf("downloaded content=%q", data)
	}
	downloadInfo, err := os.Stat(downloaded)
	if err != nil {
		t.Fatal(err)
	}
	if downloadInfo.Mode().Perm()&0o111 != 0 || !downloadInfo.ModTime().Equal(mtime) {
		t.Fatalf("download metadata mode=%o mtime=%s", downloadInfo.Mode().Perm(), downloadInfo.ModTime())
	}
	if _, err := os.Stat(rsyncTransfer); !os.IsNotExist(err) {
		t.Fatalf("rejected rsync started a transfer: %v", err)
	}
	args, err := os.ReadFile(sshArgs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), secret) {
		t.Fatalf("SSH argv leaked resolved user: %q", args)
	}
	if strings.Contains(stderr.String(), secret) || !strings.Contains(stderr.String(), diagnosticRedaction) {
		t.Fatalf("SSH diagnostics were not redacted: %q", stderr.String())
	}
}

func TestRemoteCopyArchiveUploadCleansPartialTransfer(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX archive endpoint")
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote, err := remoteCopyArchiveUploadCommand(target, "source", false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", remote)
	cmd.Stdin = strings.NewReader("truncated archive")
	if err := cmd.Run(); err == nil {
		t.Fatal("truncated transfer unexpectedly succeeded")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original" {
		t.Fatalf("partial transfer changed destination: %q", data)
	}
	matches, err := filepath.Glob(filepath.Join(parent, ".crabbox-cp*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("partial transfer left staging paths: %#v", matches)
	}
}

func TestRemoteCopyArchiveUploadCreatesTrailingSlashDestination(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX archive endpoint")
	}
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "proof.txt"), []byte("proof"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, archive, err := createCopyArchive(t.Context(), source+string(os.PathSeparator), false, defaultCopyArchiveLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func(file *os.File) {
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
	}(archive)
	target := filepath.Join(t.TempDir(), "new-destination") + string(os.PathSeparator)
	remote, err := remoteCopyArchiveUploadCommand(target, "source", true)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", remote)
	cmd.Stdin = archive
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("upload: %v: %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(target, "proof.txt"))
	if err != nil || string(data) != "proof" {
		t.Fatalf("content=%q err=%v", data, err)
	}
	_, archive, err = createCopyArchive(t.Context(), source, false, defaultCopyArchiveLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer func(file *os.File) {
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
	}(archive)
	target = filepath.Join(t.TempDir(), "new-parent") + string(os.PathSeparator)
	remote, err = remoteCopyArchiveUploadCommand(target, "source", false)
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("bash", "-c", remote)
	cmd.Stdin = archive
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("directory upload: %v: %s", err, output)
	}
	data, err = os.ReadFile(filepath.Join(target, "source", "proof.txt"))
	if err != nil || string(data) != "proof" {
		t.Fatalf("directory content=%q err=%v", data, err)
	}
	fileDestination := filepath.Join(t.TempDir(), "existing-file")
	if err := os.WriteFile(fileDestination, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote, err = remoteCopyArchiveUploadCommand(fileDestination+string(os.PathSeparator), "source", false)
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("bash", "-c", remote)
	cmd.Stdin = strings.NewReader("unused")
	if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "not a directory") {
		t.Fatalf("existing file directory destination err=%v output=%s", err, output)
	}
	symlinkDestination := filepath.Join(t.TempDir(), "directory-link")
	if err := os.Symlink(t.TempDir(), symlinkDestination); err != nil {
		t.Fatal(err)
	}
	remote, err = remoteCopyArchiveUploadCommand(symlinkDestination+string(os.PathSeparator), "source", false)
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("bash", "-c", remote)
	cmd.Stdin = strings.NewReader("unused")
	if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "symlink destination") {
		t.Fatalf("symlink directory destination err=%v output=%s", err, output)
	}
}

func TestLocalCopyArchiveTargetPreservesTrailingDirectoryIntent(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("archive fallback is unavailable on Windows operator hosts")
	}
	parent := t.TempDir()
	destination := filepath.Join(parent, "new-directory") + string(os.PathSeparator)
	target, err := localCopyArchiveTarget(destination, "source", false)
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
	if _, err := localCopyArchiveTarget(fileDestination+string(os.PathSeparator), "source", false); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("err=%v", err)
	}
}

func TestRemoteCopyArchiveCommandsAvoidLoginShells(t *testing.T) {
	upload, err := remoteCopyArchiveUploadCommand("/tmp/target", "source", false)
	if err != nil {
		t.Fatal(err)
	}
	download, err := remoteCopyArchiveDownloadCommand("/tmp/source")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{upload, download} {
		if strings.Contains(command, "bash -l") || !strings.Contains(command, "bash --noprofile --norc -c") {
			t.Fatalf("archive stream uses profile-loading shell: %s", command)
		}
	}
}

func TestRemoteCopyArchiveSourcePathIntent(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("archive fallback is unavailable on Windows operator hosts")
	}
	for _, source := range []string{"/tmp/results/.", "/tmp/results/.."} {
		if _, _, err := remoteCopyArchiveRoot(source); err == nil || !strings.Contains(err.Error(), "ending in . or ..") {
			t.Fatalf("source=%q err=%v", source, err)
		}
	}
	if hasTrailingPathSeparator(`build\`) {
		t.Fatal("POSIX backslash filename was treated as directory intent")
	}
}

func TestRemoteCopyArchiveDownloadRejectsFileWithTrailingSlash(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX archive endpoint")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("proof"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote, err := remoteCopyArchiveDownloadCommand(file + "/")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", remote)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("file source with trailing slash unexpectedly streamed")
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "not a directory") {
		t.Fatalf("stdout=%d stderr=%q", stdout.Len(), stderr.String())
	}
}

func TestRemoteCopyArchiveDownloadRejectsSocketBeforeStreaming(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX archive endpoint")
	}
	source, err := os.MkdirTemp("/tmp", "cbx-cp-socket-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(source) })
	listener, err := net.Listen("unix", filepath.Join(source, "service.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	remote, err := remoteCopyArchiveDownloadCommand(source)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", remote)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("socket-containing source unexpectedly streamed")
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "link or special file") {
		t.Fatalf("stdout=%d stderr=%q", stdout.Len(), stderr.String())
	}
}

func TestRemoteCopyArchiveDownloadRejectsHardLinksBeforeStreaming(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX archive endpoint")
	}
	source := t.TempDir()
	first := filepath.Join(source, "first")
	if err := os.WriteFile(first, []byte("proof"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, filepath.Join(source, "second")); err != nil {
		t.Fatal(err)
	}
	remote, err := remoteCopyArchiveDownloadCommand(source)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", remote)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("hard-linked source unexpectedly streamed")
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "link or special file") {
		t.Fatalf("stdout=%d stderr=%q", stdout.Len(), stderr.String())
	}
	remote, err = remoteCopyArchiveDownloadCommand(first)
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("bash", "-c", remote)
	stdout.Reset()
	stderr.Reset()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("top-level hard-linked file unexpectedly streamed")
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "link or special file") {
		t.Fatalf("top-level stdout=%d stderr=%q", stdout.Len(), stderr.String())
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
	upper, err := os.Stat(filepath.Join(stage, copyArchivePayloadRoot, "A"))
	if err != nil {
		t.Fatal(err)
	}
	lower, err := os.Stat(filepath.Join(stage, copyArchivePayloadRoot, "a"))
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

func writeExecutable(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}
