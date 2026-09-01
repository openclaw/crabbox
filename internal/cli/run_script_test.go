package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLoadRunScriptUsesContentHashedStandalonePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "check.sh")
	if err := os.WriteFile(source, []byte("#!/bin/sh\necho first\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	first, err := loadRunScript(source, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := regexp.MatchString(`^\.crabbox/scripts/[0-9a-f]{12}-check\.sh$`, filepath.ToSlash(first.RemotePath)); err != nil || !matched {
		t.Fatalf("remote path=%q, want content-hashed standalone path", first.RemotePath)
	}

	if err := os.WriteFile(source, []byte("#!/bin/sh\necho second\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	second, err := loadRunScript(source, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.RemotePath == second.RemotePath {
		t.Fatalf("remote path did not change with script content: %q", first.RemotePath)
	}
}

func TestRemoteRunScriptCommandUsesUploadedFile(t *testing.T) {
	spec := &RunScriptSpec{
		Source:     "live.sh",
		RemotePath: ".crabbox/scripts/abc-live.sh",
		Shebang:    true,
	}
	got := remoteRunScriptCommandWithEnvFile("/work/repo", map[string]string{"OPENAI_API_KEY": "sk-test"}, "", spec, []string{"arg one"})
	for _, want := range []string{
		"cd '/work/repo'",
		"OPENAI_API_KEY='sk-test'",
		"exec \"$@\"",
		"'.crabbox/scripts/abc-live.sh'",
		"'arg one'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remote command missing %q in %q", want, got)
		}
	}
}

func TestRemoteRunScriptCommandUsesWorkdirAndUploadedScriptIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell execution")
	}
	workdir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	remotePath := ".crabbox/scripts/abc-check.sh"
	fullPath := filepath.Join(workdir, filepath.FromSlash(remotePath))
	script := `#!/bin/sh
set -eu
if [ "$PWD" = "$1" ]; then echo PWD_WORKDIR=yes; fi
case "$0" in
  .crabbox/scripts/abc-check.sh|"$1"/.crabbox/scripts/abc-check.sh) echo ZERO_UPLOAD=yes ;;
esac
script_dir=$(cd "$(dirname "$0")" && pwd -P)
upload_dir=$(cd "$1/.crabbox/scripts" && pwd -P)
if [ "$script_dir" = "$upload_dir" ]; then echo DIR_UPLOAD=yes; fi
`
	upload := exec.Command("sh", "-c", "umask 022; "+remoteUploadRunScriptCommand(workdir, remotePath))
	upload.Env = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
	input, err := upload.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var uploadOutput bytes.Buffer
	upload.Stdout, upload.Stderr = &uploadOutput, &uploadOutput
	if err := upload.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = input.Close()
		if upload.ProcessState == nil {
			_ = upload.Process.Kill()
			_ = upload.Wait()
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := os.Stat(fullPath)
		if err == nil {
			if info.Mode().Perm() != 0o600 {
				t.Errorf("script is readable before upload completes: mode=%04o want=0600", info.Mode().Perm())
			}
			break
		}
		if !os.IsNotExist(err) || time.Now().After(deadline) {
			t.Fatalf("wait for upload: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := fmt.Fprint(input, script); err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	if err := upload.Wait(); err != nil {
		t.Fatalf("upload: %v\n%s", err, uploadOutput.String())
	}
	if info, err := os.Stat(fullPath); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("completed upload mode: info=%v err=%v", info, err)
	}
	spec := &RunScriptSpec{Source: "./scripts/check.sh", RemotePath: remotePath, Shebang: true}
	command := remoteRunScriptCommandWithEnvFile(workdir, nil, "", spec, []string{workdir})
	run := exec.Command("bash", "-lc", command)
	run.Env = upload.Env
	output, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("execute remote script command: %v\n%s", err, output)
	}
	for _, want := range []string{"PWD_WORKDIR=yes", "ZERO_UPLOAD=yes", "DIR_UPLOAD=yes"} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("script output missing %q:\n%s", want, output)
		}
	}
}

func TestRemoteRunScriptCommandWithoutShebangUsesBash(t *testing.T) {
	spec := &RunScriptSpec{RemotePath: ".crabbox/scripts/abc-script.sh"}
	got := remoteRunScriptCommandWithEnvFile("/work/repo", nil, "", spec, nil)
	if !strings.Contains(got, `exec bash "$@"`) {
		t.Fatalf("remote command should run script through bash: %q", got)
	}
}

func TestWindowsRunScriptForTargetUsesPowerShellExtension(t *testing.T) {
	spec := runScriptForTarget(&RunScriptSpec{RemotePath: ".crabbox/scripts/abc-script"}, SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal})
	if spec.RemotePath != ".crabbox/scripts/abc-script.ps1" {
		t.Fatalf("remote path=%q", spec.RemotePath)
	}
}

func TestWindowsRemoteRunScriptCommandUsesPowerShellFile(t *testing.T) {
	spec := &RunScriptSpec{RemotePath: `.crabbox\scripts\abc-script.ps1`}
	got := windowsRemoteRunScriptCommandWithEnvFiles(`C:\crabbox\repo`, map[string]string{"API_TOKEN": "secret"}, []string{`.crabbox\env\run.env`}, spec, []string{"arg one"})
	decoded := decodePowerShellCommand(t, got)
	for _, want := range []string{
		`Set-Location -LiteralPath 'C:\crabbox\repo'`,
		`Import-CrabboxEnvFile '.crabbox\env\run.env'`,
		`$env:API_TOKEN = 'secret'`,
		`$__crabboxScript = '.crabbox\scripts\abc-script.ps1'`,
		`$__crabboxArgs = @('arg one')`,
		`powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $__crabboxScript @__crabboxArgs`,
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("windows script command missing %q in %q", want, decoded)
		}
	}
}

func TestWindowsRemoteUploadRunScriptWritesUTF8BOMBytes(t *testing.T) {
	got := windowsRemoteUploadRunScriptCommand(`C:\crabbox\repo`, `.crabbox\scripts\abc-script.ps1`)
	decoded := decodePowerShellCommand(t, got)
	for _, want := range []string{
		`Set-Location -LiteralPath 'C:\crabbox\repo'`,
		`$stdin = [Console]::OpenStandardInput()`,
		`$stdin.CopyTo($memory)`,
		`[byte[]](0xEF, 0xBB, 0xBF)`,
		`[System.IO.File]::WriteAllBytes($fullPath, $out)`,
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("windows script upload command missing %q in %q", want, decoded)
		}
	}
	if strings.Contains(decoded, "ReadToEnd()") || strings.Contains(decoded, "WriteAllText") {
		t.Fatalf("windows script upload command should preserve bytes, got %q", decoded)
	}
}

func TestRunScriptRecordCommand(t *testing.T) {
	got := runScriptRecordCommand(&RunScriptSpec{Source: "./smoke.sh"}, []string{"--flag"})
	if strings.Join(got, " ") != "--script ./smoke.sh --flag" {
		t.Fatalf("record command=%q", got)
	}
	got = runScriptRecordCommand(&RunScriptSpec{Source: "stdin"}, nil)
	if strings.Join(got, " ") != "--script-stdin" {
		t.Fatalf("stdin record command=%q", got)
	}
}

func TestSafeScriptNameKeepsBasenameAndHash(t *testing.T) {
	for _, source := range []string{"../bad live.sh", "bad 'live.sh", "bad\nlive.sh", "bad$live.sh"} {
		got := safeScriptName(source, "abc123")
		if got != "abc123-badlive.sh" {
			t.Fatalf("source=%q safe name=%q", source, got)
		}
	}
}

func TestRunScriptFailureBundleScopesRetainedUploads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture; native Windows capture has local-only coverage")
	}
	for _, mode := range []string{"file", "stdin", "no-script", "removed", "failed-upload"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("COPYFILE_DISABLE", "1")
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			t.Chdir(dir)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					_ = conn.Close()
				}
			}()
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			ssh := `#!/bin/sh
for arg do
  if [ "$arg" = -G ]; then exec /usr/bin/ssh "$@"; fi
done
cmd=""
for arg do cmd="$arg"; done
# Decode the transport envelope so the fixture can fail an upload deliberately.
while :; do
  case "$cmd" in
    *'payload_b64="'*'"; decoded=; if command -v base64'*)
      payload=${cmd#*'payload_b64="'}
      payload=${payload%%'"; decoded=; if command -v base64'*}
      cmd=$(printf %s "$payload" | /usr/bin/base64 --decode 2>/dev/null) ||
        cmd=$(printf %s "$payload" | /usr/bin/base64 -D) ;;
    *) break ;;
  esac
done
case "$cmd" in
  *"/usr/local/bin/crabbox-ready"*) exit 0 ;;
  *"cat > "*".crabbox/scripts/"*)
    if [ "${CRABBOX_TEST_FAIL_UPLOAD:-}" = 1 ]; then exit 7; fi ;;
esac
exec sh -c "$cmd"
`
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(ssh), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "config.yaml"))
			t.Setenv("CRABBOX_FAKE_SSH_PORT", port)
			t.Setenv("CRABBOX_WORK_ROOT", filepath.Join(dir, "remote ' workspace"))
			releases := 0
			runEnvProfileTestReleaseHook = func() error { releases++; return nil }
			t.Cleanup(func() { runEnvProfileTestReleaseHook = nil })
			var stdout, stderr bytes.Buffer
			app := App{Stdout: &stdout, Stderr: &stderr}
			firstPath := filepath.Join(dir, "first success ' script.sh")
			firstData := []byte("#!/bin/sh\nprintf 'earlier-success\\n'\n")
			if err := os.WriteFile(firstPath, firstData, 0o600); err != nil {
				t.Fatal(err)
			}
			first, err := loadRunScript(firstPath, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := app.runCommand(context.Background(), []string{"--provider", "run-env-profile-test", "--no-sync", "--keep", "--script", firstPath}); err != nil {
				t.Fatalf("first run: %v\n%s", err, stderr.String())
			}
			uploads, err := filepath.Glob(filepath.Join(dir, "remote ' workspace", "*", "*", filepath.FromSlash(first.RemotePath)))
			if err != nil || len(uploads) != 1 {
				t.Fatalf("retained upload: %v %v\n%s", uploads, err, stderr.String())
			}
			store := filepath.Dir(uploads[0])
			noise := make([]byte, 8*1024*1024)
			if _, err := rand.Read(noise); err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string][]byte{"arbitrary.bin": noise, "prior.log": []byte("old log"), "junit.xml": []byte("old report"), "TEST-prior.xml": []byte("old test report")} {
				if err := os.WriteFile(filepath.Join(store, name), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			currentData := []byte("#!/bin/sh\nprintf 'current-failure\\n' >&2\nexit 23\n")
			if mode == "removed" {
				currentData = []byte("#!/bin/sh\nrm -- \"$0\"\nexit 29\n")
			}
			currentPath := filepath.Join(dir, "--current ' failure.sh")
			if err := os.WriteFile(currentPath, currentData, 0o600); err != nil {
				t.Fatal(err)
			}
			current, err := loadRunScript(currentPath, false, nil)
			if mode == "stdin" {
				current, err = loadRunScript("", true, bytes.NewReader(currentData))
				app.Stdin = bytes.NewReader(currentData)
			}
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"--provider", "run-env-profile-test", "--id", "cbx_env_profile_test", "--no-sync", "--keep-on-failure", "--timing-json"}
			switch mode {
			case "stdin":
				args = append(args, "--script-stdin")
			case "no-script":
				args = append(args, "--", "sh", "-c", "exit 23")
			default:
				args = append(args, "--script", currentPath)
			}
			if mode == "failed-upload" {
				t.Setenv("CRABBOX_TEST_FAIL_UPLOAD", "1")
			}
			stderr.Reset()
			runErr := app.runCommand(context.Background(), args)
			wantCode := 23
			if mode == "removed" {
				wantCode = 29
			}
			if mode == "failed-upload" {
				wantCode = 7
			}
			if exitCodeForError(runErr, 0) != wantCode || releases != 0 {
				t.Fatalf("failure=%v releases=%d\n%s", runErr, releases, stderr.String())
			}
			bundles, err := filepath.Glob(filepath.Join(".crabbox", "captures", "*.tar.gz"))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "failed-upload" {
				if len(bundles) != 0 || !strings.Contains(fmt.Sprint(runErr), "upload script") {
					t.Fatalf("upload failure reached capture: %v err=%v\n%s", bundles, runErr, stderr.String())
				}
				return
			}
			lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
			var timing map[string]json.RawMessage
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &timing); err != nil {
				t.Fatalf("final timing JSON: %v\n%s", err, stderr.String())
			}
			if string(timing["exitCode"]) != strconv.Itoa(wantCode) || timing["leaseStopped"] != nil || timing["leaseStopError"] != nil {
				t.Fatalf("retained reused run must omit no-attempt cleanup fields: %s", lines[len(lines)-1])
			}
			if len(bundles) != 1 {
				t.Fatalf("bundles=%v\n%s", bundles, stderr.String())
			}
			contents := readTarGzContents(t, bundles[0])
			prefix := "crabbox-artifacts/remote/"
			if mode == "file" || mode == "stdin" {
				if !bytes.Equal(contents[prefix+current.RemotePath], currentData) {
					t.Errorf("current uploaded script bytes missing: %s", current.RemotePath)
				}
			}
			for name, data := range contents {
				if strings.HasPrefix(name, prefix+".crabbox/scripts/") && (name != prefix+current.RemotePath || mode == "no-script" || mode == "removed") {
					t.Errorf("unrelated upload entry %s (%d bytes)", name, len(data))
				}
			}
			var metadata struct {
				ExitCode int `json:"exitCode"`
			}
			if err := json.Unmarshal(contents["crabbox-artifacts/crabbox-run.json"], &metadata); err != nil || metadata.ExitCode != wantCode {
				t.Fatalf("metadata=%+v err=%v", metadata, err)
			}
			if data, err := os.ReadFile(uploads[0]); err != nil || !bytes.Equal(data, firstData) {
				t.Fatal("capture modified retained earlier upload")
			}
			remoteArchives, _ := filepath.Glob(filepath.Join(filepath.Dir(store), "*.tar.gz"))
			if len(remoteArchives) != 0 {
				t.Fatalf("remote capture residue: %v", remoteArchives)
			}
			info, _ := os.Stat(bundles[0])
			t.Logf("mode=%s exit=%d retained=true bundleBytes=%d current=%s noiseBytes=%d remoteArchives=0", mode, wantCode, info.Size(), current.RemotePath, len(noise))
		})
	}
}
