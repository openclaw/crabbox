package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/crabbox/internal/runner"
	"github.com/openclaw/crabbox/internal/runner/runnerfs"
)

func TestCopyRecoveryCommandPreservesLegacyData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local archive recovery is POSIX-only")
	}
	for _, choice := range []string{"keep-destination", "restore-backup"} {
		t.Run(choice, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
			t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
			dir := t.TempDir()
			destination := filepath.Join(dir, "destination with spaces")
			for suffix, contents := range map[string]string{"": "current", ".crabbox-cp-backup": "previous", ".crabbox-cp-transaction": "owned legacy marker"} {
				if err := os.WriteFile(destination+suffix, []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			app := App{Stdout: &output, Stderr: io.Discard}
			if err := app.copyCommand(t.Context(), []string{"--recover", choice, "--", destination}); err != nil {
				t.Fatal(err)
			}
			retained, err := filepath.Glob(filepath.Join(dir, ".crabbox-cp-publish-*"))
			if err != nil || len(retained) != 1 {
				t.Fatalf("retained=%v output=%q error=%v", retained, output.String(), err)
			}
			physical, err := filepath.EvalSymlinks(retained[0])
			if err != nil || !strings.Contains(output.String(), strconv.Quote(physical)) {
				t.Fatalf("retained physical path=%q output=%q error=%v", physical, output.String(), err)
			}
			want, retainedName, retainedBytes := "current", "legacy-backup", "previous"
			if choice == "restore-backup" {
				want, retainedName, retainedBytes = "previous", "legacy-destination", "current"
			}
			for name, expected := range map[string]string{destination: want, filepath.Join(retained[0], retainedName): retainedBytes, filepath.Join(retained[0], "legacy-marker"): "owned legacy marker"} {
				data, err := os.ReadFile(name)
				if err != nil || string(data) != expected {
					t.Fatalf("read %s=%q error=%v", name, data, err)
				}
			}
		})
	}
}

func TestCopyRecoveryRejectsAmbiguousArguments(t *testing.T) {
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	for _, args := range [][]string{
		{"--recover=", "destination"}, {"--recover", "unknown", "destination"},
		{"--recover", "keep-destination", "a", "b"},
		{"--recover", "keep-destination", "--id", "lease", "local"},
		{"--recover", "keep-destination", "--provider", "ssh", "local"},
		{"--recover", "keep-destination", "-L=false", "destination"},
		{"--recover", "keep-destination", "SANDBOX:/destination"},
	} {
		if err := app.copyCommand(t.Context(), args); err == nil {
			t.Fatalf("accepted ambiguous arguments %q", args)
		}
	}
}

func TestArchiveRecoveryGuidanceKeepsDestinationScope(t *testing.T) {
	local := &runnerfs.ArchiveAdoptionRequiredError{Target: "/tmp/destination with spaces"}
	if err := archiveRecoveryGuidance(local, "lease"); !strings.Contains(err.Error(), "--recover keep-destination -- ") || strings.Contains(err.Error(), "--id") || !errors.Is(err, local) {
		t.Fatalf("local guidance: %v", err)
	}
	remote := runner.RemoteError{Code: "recovery-required", Target: "/tmp/remote destination", Message: "recovery needed"}
	if err := archiveRecoveryGuidance(remote, "lease"); !strings.Contains(err.Error(), "--id ") || !strings.Contains(err.Error(), "SANDBOX:/tmp/remote destination") || !strings.Contains(err.Error(), "restore-backup") {
		t.Fatalf("remote guidance: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := recoverLocalArchive(ctx, "unused", runnerfs.ArchiveKeepDestination, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
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
	target := SSHTarget{TargetOS: targetLinux, User: secret, Host: "example.test", Port: "22", AuthSecret: true}
	if runtime.GOOS == "darwin" {
		target.TargetOS = targetMacOS
	}
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
	// A normal refused copy still retires its executable. Explicit recovery
	// preserves the old data rather than inferring authority from its marker.
	remoteTarget := filepath.Join(remoteDestination, filepath.Base(source))
	if err := os.WriteFile(remoteTarget+".crabbox-cp-transaction", []byte("owned legacy marker"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remoteTarget+".crabbox-cp-backup", []byte("previous data"), 0600); err != nil {
		t.Fatal(err)
	}
	err = copyOverResolvedSSH(t.Context(), target, source, "SANDBOX:"+remoteDestination, false, io.Discard, &stderr)
	var recovery runner.RemoteError
	if !errors.As(err, &recovery) || recovery.Code != "recovery-required" || strings.Contains(err.Error(), "retirement unconfirmed") {
		t.Fatalf("ordinary refusal: %v", err)
	}
	var recovered bytes.Buffer
	if err := recoverRemoteArchive(t.Context(), target, recovery.Target, runnerfs.ArchiveKeepDestination, &recovered, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recovered.String(), "Recovery complete on lease") {
		t.Fatalf("recovery result: %s", recovered.String())
	}
	retained, err := filepath.Glob(filepath.Join(remoteDestination, ".crabbox-cp-publish-*"))
	if err != nil || len(retained) != 1 {
		t.Fatalf("retained=%v error=%v", retained, err)
	}
	previous, err := os.ReadFile(filepath.Join(retained[0], "legacy-backup"))
	if err != nil || string(previous) != "previous data" {
		t.Fatalf("retained backup=%q error=%v", previous, err)
	}
	commands, err := os.ReadFile(sshArgs)
	if err != nil {
		t.Fatal(err)
	}
	stages := regexp.MustCompile(`/tmp/crabbox-runtime-[0-9a-f]{32}`).FindAllString(string(commands), -1)
	if len(stages) == 0 {
		t.Fatal("fixture did not observe installed runtime paths")
	}
	for _, stage := range stages {
		if _, err := os.Lstat(stage); !os.IsNotExist(err) {
			t.Fatalf("runtime stage remains after completed operation: %s (%v)", stage, err)
		}
	}
}
