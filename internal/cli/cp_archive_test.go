package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

func writeExecutable(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}
