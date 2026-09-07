package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestResolvedSSHCopyCommandUsesNativeWindowsSiblingPair(t *testing.T) {
	toolDir := filepath.Join(t.TempDir(), "MSYS2 tools")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"rsync.exe", "ssh.exe"} {
		if err := os.WriteFile(filepath.Join(toolDir, name), []byte("fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", toolDir)
	t.Setenv("CRABBOX_TRANSFER_DENIED", "ambient-sentinel")
	t.Setenv("CRABBOX_TRANSFER_OVERRIDE", "ambient-sentinel")
	target := SSHTarget{ChildEnvDenylist: []string{"CRABBOX_TRANSFER_DENIED"}, ChildEnv: map[string]string{"CRABBOX_TRANSFER_OVERRIDE": "target-sentinel"}}
	handle, err := resolvedRsyncCommandForGOOS(context.Background(), "windows", target, []string{
		"-az", "-e", "'ssh' '-F' 'private config'", "--", "source", "target:dest",
	}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	execHandle := handle
	if execHandle.cmd.Path != filepath.Join(toolDir, "rsync.exe") {
		t.Fatalf("rsync path=%q", execHandle.cmd.Path)
	}
	wantSSH := rsyncShellWords([]string{filepath.Join(toolDir, "ssh.exe")})[0]
	if got := execHandle.cmd.Args[3]; !strings.HasPrefix(got, wantSSH+" ") || !strings.Contains(got, "'private config'") {
		t.Fatalf("paired remote shell=%q", got)
	}
	joinedEnv := strings.Join(execHandle.cmd.Env, "\n")
	if !strings.Contains(joinedEnv, "MSYS2_ARG_CONV_EXCL=*") || !strings.Contains(joinedEnv, "MSYS_NO_PATHCONV=1") || !strings.Contains(joinedEnv, "CYGWIN=nodosfilewarning") {
		t.Fatal("native rsync conversion environment missing required settings")
	}
	if slices.Contains(handle.cmd.Env, "CRABBOX_TRANSFER_DENIED=ambient-sentinel") || !slices.Contains(handle.cmd.Env, "CRABBOX_TRANSFER_OVERRIDE=target-sentinel") {
		t.Fatal("native rsync lost target environment policy")
	}
}

func TestResolvedSSHCopyCommandFailsClosedWithoutNativeWindowsSibling(t *testing.T) {
	toolDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(toolDir, "rsync.exe"), []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", toolDir)
	_, err := resolvedRsyncCommandForGOOS(context.Background(), "windows", SSHTarget{}, []string{
		"-az", "-e", "'ssh' '-F' 'private config'", "--", "source", "target:dest",
	}, "", "")
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(err.Error(), "sibling OpenSSH") || !strings.Contains(err.Error(), "WSL2") {
		t.Fatalf("diagnostic=%q", err)
	}
}

func TestResolvedSSHCopyArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	session, err := newSSHTransportSession(t.Context(), SSHTarget{User: "alice", Host: "example.test", Port: "22"}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	upload, err := resolvedSSHCopyArgs(session, SSHTarget{}, "./source dir", "SANDBOX:/tmp/destination file", true, false)
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
	download, err := resolvedSSHCopyArgs(session, SSHTarget{}, "SANDBOX:/tmp/result.log", "./result.log", true, false)
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
	for _, option := range []string{"-rtz", "--no-links", "--no-devices", "--no-specials", "--no-owner", "--no-group", "--no-perms", "--chmod=Du=rwx,Dgo=rx,Fu=rw,Fgo=r"} {
		if !containsString(download, option) {
			t.Fatalf("safe download args missing %s: %#v", option, download)
		}
	}
	if containsString(download, "-az") {
		t.Fatalf("download must not inherit archive receive semantics: %#v", download)
	}
}

func TestResolvedSSHCopyArgsEscapesRemotePatterns(t *testing.T) {
	session := &sshTransportSession{configPath: "/private/config"}
	args, err := resolvedSSHCopyArgs(session, SSHTarget{}, "SANDBOX:/tmp/result[1]*?.json", "result.json", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := args[len(args)-2]; got != sshTransportHostAlias+`:/tmp/result\[1]\*\?.json` {
		t.Fatalf("remote operand=%q", got)
	}
}

func TestResolvedSSHCopyArgsSecludedTransport(t *testing.T) {
	session := &sshTransportSession{configPath: "/private/config"}
	download, err := resolvedSSHCopyArgs(session, SSHTarget{}, "SANDBOX:/tmp/result[1]*?.json", "./output", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(download, "--secluded-args") || containsString(download, "--no-secluded-args") {
		t.Fatalf("secluded download args=%#v", download)
	}
	// The server-side globbing of secluded args still honors backslash
	// escapes, so the source keeps the literal-wildcard escaping.
	if got := download[len(download)-2]; got != sshTransportHostAlias+`:/tmp/result\[1]\*\?.json` {
		t.Fatalf("secluded remote source=%q", got)
	}
	if containsString(download, "--rsync-path") {
		t.Fatalf("non-WSL2 secluded args must not override --rsync-path: %#v", download)
	}
	upload, err := resolvedSSHCopyArgs(session, SSHTarget{}, "./input", "SANDBOX:/tmp/result[1]*?.json", false, true)
	if err != nil {
		t.Fatal(err)
	}
	// Secluded destinations bypass the remote shell entirely, so they stay
	// unescaped exactly like the WSL2 secluded path.
	if got := upload[len(upload)-1]; got != sshTransportHostAlias+`:/tmp/result[1]*?.json` {
		t.Fatalf("secluded destination operand=%q", got)
	}
}

func TestResolvedSSHCopyArgsSecludedSourceEscaping(t *testing.T) {
	session := &sshTransportSession{configPath: "/private/config"}
	tests := map[string]string{
		`SANDBOX:/tmp/foo\bar[1].json`:   sshTransportHostAlias + `:/tmp/foo\\bar\[1].json`,
		`SANDBOX:/tmp/foo\bar/result[1]`: sshTransportHostAlias + `:/tmp/foo\bar/result\[1]`,
	}
	for source, want := range tests {
		args, err := resolvedSSHCopyArgs(session, SSHTarget{}, source, "./output", false, true)
		if err != nil {
			t.Fatal(err)
		}
		if got := args[len(args)-2]; got != want {
			t.Errorf("secluded remote source=%q, want %q", got, want)
		}
	}
}

func TestResolvedSSHCopyArgsPreservesCurrentUserTildeExpansion(t *testing.T) {
	session := &sshTransportSession{configPath: "/private/config"}
	tests := []struct {
		name        string
		source      string
		destination string
		remoteArg   int
	}{
		{name: "download", source: "SANDBOX:~/result.log", destination: "./output", remoteArg: -2},
		{name: "upload", source: "./input", destination: "SANDBOX:~/result.log", remoteArg: -1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args, err := resolvedSSHCopyArgs(session, SSHTarget{}, test.source, test.destination, false, true)
			if err != nil {
				t.Fatal(err)
			}
			if !containsString(args, "--no-secluded-args") || containsString(args, "--secluded-args") {
				t.Fatalf("current-user tilde path must use shell-transported args: %#v", args)
			}
			if got := args[len(args)+test.remoteArg]; got != sshTransportHostAlias+":~/result.log" {
				t.Fatalf("remote path=%q", got)
			}
		})
	}
}

func TestResolvedSSHCopyArgsPreservesNamedUserTildeExpansion(t *testing.T) {
	session := &sshTransportSession{configPath: "/private/config"}
	args, err := resolvedSSHCopyArgs(session, SSHTarget{}, "SANDBOX:~alice/result.log", "./output", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(args, "--no-secluded-args") || containsString(args, "--secluded-args") {
		t.Fatalf("named-user tilde path must use shell-transported args: %#v", args)
	}
	if got := args[len(args)-2]; got != sshTransportHostAlias+":~alice/result.log" {
		t.Fatalf("remote source=%q", got)
	}
	for _, source := range []string{"SANDBOX:~alice/result[1].log", `SANDBOX:~alice/file\]name`} {
		_, err = resolvedSSHCopyArgs(session, SSHTarget{}, source, "./output", false, true)
		if err == nil || !strings.Contains(err.Error(), "use an absolute path") {
			t.Fatalf("unsafe named-user path=%q err=%v", source, err)
		}
	}
}

func TestResolvedSSHCopyArgsRejectsBareRemoteHomeDownloads(t *testing.T) {
	session := &sshTransportSession{configPath: "/private/config"}
	for _, source := range []string{"SANDBOX:~", "SANDBOX:~alice"} {
		_, err := resolvedSSHCopyArgs(session, SSHTarget{}, source, "./output", false, true)
		if err == nil || !strings.Contains(err.Error(), "use a path under ~/ or an absolute path") {
			t.Errorf("source=%q err=%v", source, err)
		}
	}
}

func TestResolvedSSHCopyArgsKeepsLeadingColonInRemotePath(t *testing.T) {
	session := &sshTransportSession{configPath: "/private/config"}
	args, err := resolvedSSHCopyArgs(session, SSHTarget{}, "SANDBOX::result", "result", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := args[len(args)-2]; got != sshTransportHostAlias+`:./:result` {
		t.Fatalf("remote operand=%q", got)
	}
}

func TestResolvedSSHCopyArgsRejectsRemoteControlCharacters(t *testing.T) {
	session := &sshTransportSession{configPath: "/private/config"}
	_, err := resolvedSSHCopyArgs(session, SSHTarget{}, "SANDBOX:/tmp/result\nnext", "result", false, false)
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
	args, err := resolvedSSHCopyArgs(session, SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, "./input", "SANDBOX:/tmp/input", false, false)
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
	uploadPatternArgs, err := resolvedSSHCopyArgs(session, SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, "./input", "SANDBOX:/tmp/result[1]*?.json", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := uploadPatternArgs[len(uploadPatternArgs)-1]; got != sshTransportHostAlias+`:/tmp/result[1]*?.json` {
		t.Fatalf("WSL2 destination operand=%q", got)
	}
	patternArgs, err := resolvedSSHCopyArgs(session, SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, "SANDBOX:/tmp/result[1]*?.json", "./output", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := patternArgs[len(patternArgs)-2]; got != sshTransportHostAlias+`:/tmp/result\[1]\*\?.json` {
		t.Fatalf("WSL2 remote operand=%q", got)
	}
}

func TestSSHCopyUsesNativeWindowsTransportForConfigProxy(t *testing.T) {
	if sshTransferUsesWSL("windows", SSHTarget{SSHConfigProxy: true}) {
		t.Fatal("Windows SSH config proxy must use the native OpenSSH config")
	}
	if !sshTransferUsesWSL("windows", SSHTarget{}) {
		t.Fatal("ordinary Windows SSH copy should use WSL rsync")
	}
}

func TestResolvedSSHCopyFallsBackFromUnsafeWSLRsync(t *testing.T) {
	unsafeWSL := resolvedRsyncCapabilities{version: "3.2.7"}
	safeNative := resolvedRsyncCapabilities{version: "3.4.4", safeTransport: true}
	if !preferNativeResolvedRsync(unsafeWSL, safeNative) {
		t.Fatal("safe native rsync should replace an unsafe WSL rsync")
	}
	if preferNativeResolvedRsync(unsafeWSL, resolvedRsyncCapabilities{version: "3.4.2"}) {
		t.Fatal("an unsafe native rsync must not replace WSL")
	}
	if preferNativeResolvedRsync(safeNative, safeNative) {
		t.Fatal("safe WSL rsync remains preferred")
	}
}

func TestResolvedSSHCopyArgsTreatsColonPathAsLocal(t *testing.T) {
	session := &sshTransportSession{configPath: "/private/config"}
	args, err := resolvedSSHCopyArgs(session, SSHTarget{}, "report:final", "SANDBOX:/tmp/report", false, false)
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
	installFakeSecludedArgsProbeSSH(t, dir, 1)
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
	if !strings.Contains(got, "--no-secluded-args") {
		t.Fatalf("probe failure must fall back to shell-transported args: %q", got)
	}
	if strings.Contains(stderr.String(), target.User) || !strings.Contains(stderr.String(), diagnosticRedaction) {
		t.Fatalf("SSH diagnostics were not redacted: %q", stderr.String())
	}
}

// installFakeSecludedArgsProbeSSH writes an ssh shim into dir that answers the
// secluded-args capability probe with probeExit and delegates every other
// invocation (config resolution, feature probes, the transfer itself) to the
// real ssh binary so the transport session behaves normally.
func installFakeSecludedArgsProbeSSH(t *testing.T, dir string, probeExit int) {
	t.Helper()
	realSSH, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
case " $* " in
  *" rsync --protect-args --version "*) exit ` + strconv.Itoa(probeExit) + ` ;;
esac
exec "` + realSSH + `" "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestCopyOverResolvedSSHPrefersSecludedArgsWhenRemoteSupportsThem(t *testing.T) {
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
`
	if err := os.WriteFile(rsyncPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	installFakeSecludedArgsProbeSSH(t, dir, 0)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_TEST_RSYNC_ARGS", capture)
	t.Setenv("HOME", t.TempDir())
	target := SSHTarget{User: "alice", Host: "example.test", Port: "22"}
	if err := copyOverResolvedSSH(t.Context(), target, "./input.txt", "SANDBOX:/tmp/input.txt", false, os.Stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "--secluded-args") || strings.Contains(got, "--no-secluded-args") {
		t.Fatalf("supported secluded args were not preferred: %q", got)
	}
}

func TestCopyOverResolvedSSHRejectsNativeWindows(t *testing.T) {
	err := copyOverResolvedSSH(t.Context(), SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, "./a", "SANDBOX:C:/a", false, os.Stdout, os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "native Windows") {
		t.Fatalf("err=%v", err)
	}
}

func TestCopyOverResolvedSSHFallsBackWithoutStartingUnsafeRsync(t *testing.T) {
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
	writeExecutable(t, filepath.Join(dir, "ssh"), "#!/bin/sh\nexit 86\n")
	input := filepath.Join(dir, "input")
	if err := os.WriteFile(input, []byte("archive fallback"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRABBOX_TEST_RSYNC_TRANSFER_MARKER", transferMarker)
	err := copyOverResolvedSSH(t.Context(), SSHTarget{User: "alice", Host: "example.test", Port: "22"}, input, "SANDBOX:/tmp/input", false, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || strings.Contains(err.Error(), "rsync 3.4.3 or newer") {
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

func TestResolvedSSHCopyWSLArgsConvertsOnlyLocalOperands(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "download preserves escaped remote wildcards",
			args: []string{"-az", "-e", "ssh -F /tmp/config", "--", sshTransportHostAlias + `:/tmp/result\[1\].json`, "/c/Users/alice/result.json"},
			want: []string{"-az", "-e", "ssh -F /tmp/config", "--", sshTransportHostAlias + `:/tmp/result\[1\].json`, "/mnt/c/Users/alice/result.json"},
		},
		{
			name: "upload preserves remote backslashes",
			args: []string{"-az", "--", "/d/work/input.txt", sshTransportHostAlias + `:/tmp/name\\with\\slashes`},
			want: []string{"-az", "--", "/mnt/d/work/input.txt", sshTransportHostAlias + `:/tmp/name\\with\\slashes`},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := resolvedRsyncWSLArgs(test.args, "/mnt"); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("args=%#v, want %#v", got, test.want)
			}
		})
	}
}

func TestRedactSSHTransportDiagnostic(t *testing.T) {
	target := SSHTarget{User: "opaque-user-value", AuthSecret: true}
	got := redactSSHTransportDiagnostic(target, "opaque-user-value@example.test: Permission denied")
	if strings.Contains(got, target.User) || !strings.Contains(got, diagnosticRedaction) {
		t.Fatalf("diagnostic=%q", got)
	}
}
