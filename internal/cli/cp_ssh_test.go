package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolvedSSHCopyArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	session, err := newSSHTransportSession(t.Context(), SSHTarget{User: "alice", Host: "example.test", Port: "22"}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	upload, err := resolvedSSHCopyArgs(session, SSHTarget{}, "./source dir", "SANDBOX:/tmp/destination file", true)
	if err != nil {
		t.Fatal(err)
	}
	wantUploadSuffix := []string{"--copy-links", "--", "./source dir", sshTransportHostAlias + ":/tmp/destination file"}
	if !reflect.DeepEqual(upload[len(upload)-len(wantUploadSuffix):], wantUploadSuffix) {
		t.Fatalf("upload args=%#v", upload)
	}
	for _, option := range []string{"--no-old-args", "--no-secluded-args"} {
		if !containsString(upload, option) {
			t.Fatalf("safe rsync args missing %s: %#v", option, upload)
		}
	}
	download, err := resolvedSSHCopyArgs(session, SSHTarget{}, "SANDBOX:/tmp/result.log", "./result.log", true)
	if err != nil {
		t.Fatal(err)
	}
	wantDownloadSuffix := []string{"--", sshTransportHostAlias + ":/tmp/result.log", "./result.log"}
	if !reflect.DeepEqual(download[len(download)-len(wantDownloadSuffix):], wantDownloadSuffix) {
		t.Fatalf("download args=%#v", download)
	}
	if containsString(download, "--copy-links") {
		t.Fatalf("download should not apply host-side -L: %#v", download)
	}
}

func TestResolvedSSHCopyArgsEscapesRemotePatterns(t *testing.T) {
	session := &sshTransportSession{configPath: "/private/config"}
	args, err := resolvedSSHCopyArgs(session, SSHTarget{}, "SANDBOX:/tmp/result[1]*?.json", "result.json", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := args[len(args)-2]; got != sshTransportHostAlias+`:/tmp/result\[1]\*\?.json` {
		t.Fatalf("remote operand=%q", got)
	}
}

func TestResolvedSSHCopyArgsKeepsLeadingColonInRemotePath(t *testing.T) {
	session := &sshTransportSession{configPath: "/private/config"}
	args, err := resolvedSSHCopyArgs(session, SSHTarget{}, "SANDBOX::result", "result", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := args[len(args)-2]; got != sshTransportHostAlias+`:./:result` {
		t.Fatalf("remote operand=%q", got)
	}
}

func TestResolvedSSHCopyArgsRejectsRemoteControlCharacters(t *testing.T) {
	session := &sshTransportSession{configPath: "/private/config"}
	_, err := resolvedSSHCopyArgs(session, SSHTarget{}, "SANDBOX:/tmp/result\nnext", "result", false)
	if err == nil || !strings.Contains(err.Error(), "control characters") {
		t.Fatalf("err=%v", err)
	}
}

func TestRsyncRemoteCopyPathPreservesLiteralBackslash(t *testing.T) {
	if got := rsyncRemoteCopyPath(`a\b`); got != `a\b` {
		t.Fatalf("remote path=%q", got)
	}
	if got := rsyncRemoteCopyPath(`/tmp/result[1].json`); got != `/tmp/result\[1].json` {
		t.Fatalf("secluded remote path=%q", got)
	}
	if got := rsyncRemoteCopyPath(`/tmp/result]1.json`); got != `/tmp/result]1.json` {
		t.Fatalf("standalone closing bracket path=%q", got)
	}
	if got := rsyncRemoteCopyPath(`/tmp/a\*`); got != `/tmp/a\\\*` {
		t.Fatalf("backslash wildcard path=%q", got)
	}
}

func TestResolvedSSHCopyArgsSelectsWSLRemoteRsync(t *testing.T) {
	session := &sshTransportSession{configPath: "/private/config"}
	args, err := resolvedSSHCopyArgs(session, SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, "./input", "SANDBOX:/tmp/input", false)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	if !strings.Contains(got, "--rsync-path wsl.exe rsync") {
		t.Fatalf("args=%q", got)
	}
	if !containsString(args, "--secluded-args") || containsString(args, "--no-secluded-args") {
		t.Fatalf("WSL2 rsync args=%#v", args)
	}
	uploadPatternArgs, err := resolvedSSHCopyArgs(session, SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, "./input", "SANDBOX:/tmp/result[1]*?.json", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := uploadPatternArgs[len(uploadPatternArgs)-1]; got != sshTransportHostAlias+`:/tmp/result[1]*?.json` {
		t.Fatalf("WSL2 destination operand=%q", got)
	}
	patternArgs, err := resolvedSSHCopyArgs(session, SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, "SANDBOX:/tmp/result[1]*?.json", "./output", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := patternArgs[len(patternArgs)-2]; got != sshTransportHostAlias+`:/tmp/result\[1]\*\?.json` {
		t.Fatalf("WSL2 remote operand=%q", got)
	}
}

func TestSSHCopyUsesNativeWindowsTransportForConfigProxy(t *testing.T) {
	if sshCopyUsesWSL("windows", SSHTarget{SSHConfigProxy: true}) {
		t.Fatal("Windows SSH config proxy must use the native OpenSSH config")
	}
	if !sshCopyUsesWSL("windows", SSHTarget{}) {
		t.Fatal("ordinary Windows SSH copy should use WSL rsync")
	}
}

func TestResolvedSSHCopyArgsTreatsColonPathAsLocal(t *testing.T) {
	session := &sshTransportSession{configPath: "/private/config"}
	args, err := resolvedSSHCopyArgs(session, SSHTarget{}, "report:final", "SANDBOX:/tmp/report", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := args[len(args)-2]; got != "./report:final" {
		t.Fatalf("local operand=%q", got)
	}
}

func TestRsyncCopyLocalPathPreservesWindowsAbsolutePaths(t *testing.T) {
	tests := map[string]string{
		`C:\work\file.txt`: `/c/work/file.txt`,
		`D:/work/file.txt`: `/d/work/file.txt`,
		`report:final`:     `./report:final`,
		`relative/file`:    `./relative/file`,
	}
	for path, want := range tests {
		if got := rsyncCopyLocalPathForGOOS("windows", path); got != want {
			t.Errorf("rsyncCopyLocalPathForGOOS(%q)=%q want %q", path, got, want)
		}
	}
}

func TestCopyOverResolvedSSHInvokesRsyncWithoutResolvedCredentials(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX fake rsync helper")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "args")
	rsyncPath := filepath.Join(dir, "rsync")
	script := `#!/bin/sh
set -eu
case " $* " in
  *" --version "*) printf 'rsync  version 3.4.4  protocol version 32\n'; exit 0 ;;
esac
printf '%s\n' "$@" > "$CRABBOX_TEST_RSYNC_ARGS"
printf 'opaque-user-value@example.test: Permission denied\n' >&2
`
	if err := os.WriteFile(rsyncPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_TEST_RSYNC_ARGS", capture)
	t.Setenv("HOME", t.TempDir())
	target := SSHTarget{User: "opaque-user-value", Host: "example.test", Port: "22", AuthSecret: true}
	var stderr bytes.Buffer
	if err := copyOverResolvedSSH(t.Context(), target, "./input.txt", "SANDBOX:/tmp/input.txt", false, os.Stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, target.User) {
		t.Fatalf("rsync argv leaked resolved user: %q", got)
	}
	if !strings.Contains(got, sshTransportHostAlias+":/tmp/input.txt") {
		t.Fatalf("rsync argv=%q", got)
	}
	if strings.Contains(stderr.String(), target.User) || !strings.Contains(stderr.String(), diagnosticRedaction) {
		t.Fatalf("SSH diagnostics were not redacted: %q", stderr.String())
	}
}

func TestCopyOverResolvedSSHRejectsNativeWindows(t *testing.T) {
	err := copyOverResolvedSSH(t.Context(), SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, "./a", "SANDBOX:C:/a", false, os.Stdout, os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "native Windows") {
		t.Fatalf("err=%v", err)
	}
}

func TestCopyOverResolvedSSHRejectsUnsafeRsyncBeforeTransfer(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX fake rsync helper")
	}
	dir := t.TempDir()
	transferMarker := filepath.Join(dir, "transfer-started")
	rsyncPath := filepath.Join(dir, "rsync")
	script := `#!/bin/sh
set -eu
case " $* " in
  *" --version "*) printf 'rsync  version 3.4.2  protocol version 32\n'; exit 0 ;;
esac
printf started > "$CRABBOX_TEST_RSYNC_TRANSFER_MARKER"
`
	if err := os.WriteFile(rsyncPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRABBOX_TEST_RSYNC_TRANSFER_MARKER", transferMarker)
	err := copyOverResolvedSSH(t.Context(), SSHTarget{User: "alice", Host: "example.test", Port: "22"}, "./input", "SANDBOX:/tmp/input", false, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "rsync 3.4.3 or newer") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(transferMarker); !os.IsNotExist(err) {
		t.Fatalf("unsafe rsync started a transfer: %v", err)
	}
}

func TestParseRsyncVersionSafetyBoundary(t *testing.T) {
	tests := []struct {
		output string
		want   string
		ok     bool
		safe   bool
	}{
		{output: "rsync  version 3.4.2  protocol version 32", want: "3.4.2", ok: true},
		{output: "rsync  version 3.4.3  protocol version 32", want: "3.4.3", ok: true, safe: true},
		{output: "rsync  version 3.4.3-1  protocol version 32", want: "3.4.3-1", ok: true, safe: true},
		{output: "rsync  version 3.4.3pre1  protocol version 32", want: "3.4.3pre1"},
		{output: "rsync  version 3.4.3-rc1  protocol version 32", want: "3.4.3-rc1"},
		{output: "openrsync: protocol version 29\nrsync version 2.6.9 compatible", want: "2.6.9", ok: true},
	}
	for _, test := range tests {
		major, minor, patch, version, ok := parseRsyncVersion(test.output)
		if ok != test.ok || version != test.want {
			t.Fatalf("parse %q: version=%q ok=%t", test.output, version, ok)
		}
		if safe := ok && rsyncVersionAtLeast(major, minor, patch, 3, 4, 3); safe != test.safe {
			t.Fatalf("version %s safe=%t want=%t", version, safe, test.safe)
		}
	}
}

func TestRedactSSHTransportDiagnostic(t *testing.T) {
	target := SSHTarget{User: "opaque-user-value", AuthSecret: true}
	got := redactSSHTransportDiagnostic(target, "opaque-user-value@example.test: Permission denied")
	if strings.Contains(got, target.User) || !strings.Contains(got, diagnosticRedaction) {
		t.Fatalf("diagnostic=%q", got)
	}
}
