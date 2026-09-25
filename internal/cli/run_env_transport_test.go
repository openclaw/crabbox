package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSSHCommandEnvDeliveryAndCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH fixture")
	}
	for _, outcome := range []string{"success", "failure", "cancel", "upload-failure"} {
		t.Run(outcome, func(t *testing.T) {
			isolateTestUserDirs(t)
			dir := t.TempDir()
			logPath := filepath.Join(dir, "ssh.argv")
			t.Setenv("CRABBOX_ENV_ARGV_LOG", logPath)
			t.Setenv("CRABBOX_ENV_UPLOAD_FAIL", outcome)
			mustWriteTestFile(t, filepath.Join(dir, "ssh"), `#!/bin/sh
for arg do remote=$arg; done
printf '%s\n' "$@" >> "$CRABBOX_ENV_ARGV_LOG"
/bin/sh -c "$remote"
code=$?
case "$remote:$CRABBOX_ENV_UPLOAD_FAIL" in
  *'cat > '*:upload-failure) printf 'allowlisted-env-canary-2535' >&2; exit 7 ;;
esac
exit "$code"
`)
			if err := os.Chmod(filepath.Join(dir, "ssh"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			const canary = "allowlisted-env-canary-2535"
			value := canary + " '\" $literal `literal`\nsecond\rline ☃ "
			values := map[string]string{"TEST_VALUE": value, "TEST_EMPTY": "", "INVALID-NAME": "ignored"}
			target := SSHTarget{Host: "fixture.invalid", User: "fixture", Port: "22", TargetOS: targetLinux}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var diagnostics bytes.Buffer
			prepared, err := stageSSHCommandEnv(ctx, target, dir, values, &diagnostics)
			if outcome == "upload-failure" {
				if err == nil || strings.Contains(err.Error(), canary) {
					t.Fatalf("upload failure was not safely reported: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(filepath.Join(dir, prepared.File))
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("private env file permissions: %v, %v", info, err)
				}
				profile := filepath.Join(dir, "actions.env")
				mustWriteTestFile(t, profile, "cd /\n")
				command := remoteCommandWithEnvFiles(dir, nil, []string{profile, prepared.File}, []string{"/bin/sh", "-c", `test "${TEST_EMPTY+set}" = set && printf '%s' "$TEST_VALUE"`})
				owner := &workspaceOwner{key: "fixture", token: "fixture"}
				for _, text := range []string{command, owner.wrapPOSIXCommand(command, false)} {
					if strings.Contains(text, canary) || strings.Contains(text, base64.StdEncoding.EncodeToString([]byte(canary))) {
						t.Fatal("command or workspace-owner wrapper contains an env value")
					}
				}
				var output bytes.Buffer
				if _, err := runSSHStreamResult(ctx, target, command, &output, &diagnostics); err != nil || output.String() != value {
					t.Fatalf("remote environment round trip failed: %v", err)
				}
				if outcome == "failure" {
					if code, _ := runSSHStreamResult(ctx, target, remoteCommandWithEnvFiles(dir, nil, []string{prepared.File}, []string{"false"}), io.Discard, &diagnostics); code == 0 {
						t.Fatal("workload failure lost")
					}
				}
				if outcome == "cancel" {
					cancel()
				}
				prepared.close()
			}
			if _, err := os.Stat(filepath.Join(dir, shellDir(prepared.File))); !os.IsNotExist(err) {
				t.Fatalf("private env directory survived cleanup: %v", err)
			}
			argv, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(argv, []byte(canary)) || strings.Contains(diagnostics.String(), canary) {
				t.Fatal("canary leaked into argv or diagnostics")
			}
		})
	}
}

func TestSSHCommandEnvWindowsRoundTrip(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell unavailable")
	}
	dir := t.TempDir()
	path := ".crabbox/env-fixture/values.ps1"
	const value = "allowlisted-env-canary-2535 '\" $literal `literal`\r\n☃ "
	mustWriteTestFile(t, filepath.Join(dir, filepath.FromSlash(path)), formatPowerShellCommandEnv(map[string]string{"TEST_VALUE": value}))
	remote := windowsRemoteShellCommandWithEnvFiles(dir, nil, []string{path}, `[Console]::Write($env:TEST_VALUE)`)
	decoded := decodePowerShellCommand(t, remote)
	if strings.Contains(decoded, value) {
		t.Fatal("native Windows command contains env value")
	}
	cmd := exec.CommandContext(t.Context(), pwsh, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", decoded)
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != value {
		t.Fatalf("Windows env round trip: %v, %q", err, out)
	}
	upload := decodePowerShellCommand(t, uploadSSHCommandEnvCommand(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, dir, path))
	for _, want := range []string{"SetAccessRuleProtection($true, $false)", "Set-Acl -LiteralPath $dir", "[IO.FileMode]::CreateNew", "OpenStandardInput().CopyTo($file)"} {
		if !strings.Contains(upload, want) {
			t.Fatalf("Windows private upload missing %s", want)
		}
	}
	if strings.Index(upload, "Set-Acl") > strings.Index(upload, "[IO.File]::Open") {
		t.Fatal("Windows upload writes before restricting access")
	}
}

func TestSSHCommandEnvFailureMetadataRedactsCanary(t *testing.T) {
	const canary = "allowlisted-env-canary-2535"
	var bundle bytes.Buffer
	w := tar.NewWriter(&bundle)
	if err := addFailureBundleMetadata(w, FailureCaptureMetadata{EnvAllow: []string{"TEST_VALUE"}, Env: map[string]string{"TEST_VALUE": canary}, CommandDisplay: "probe " + canary}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bundle.Bytes(), []byte(canary)) {
		t.Fatal("failure metadata/timing leaked allowlisted value")
	}
}

func TestSSHCommandEnvWSLSeparatesPayloadFromCommand(t *testing.T) {
	const canary = "allowlisted-env-canary-2535"
	target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	command := uploadSSHCommandEnvCommand(target, "/work/repo", ".crabbox/env-fixture/values.sh")
	input := formatShellEnvFile(map[string]string{"TEST_VALUE": canary})
	transport, err := prepareSSHTransport(t.Context(), target, command, strings.NewReader(input), int64(len(input)), sshCommandLimit{})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.close()
	if transport.stage == nil {
		t.Fatal("WSL upload did not select the staged transport")
	}
	reader, err := transport.stage.input.reset()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	_, _, stagedCommand, payload := decodeWSLStage(t, data)
	if stagedCommand != command || strings.Contains(stagedCommand, canary) || string(payload) != input {
		t.Fatal("WSL transport mixed environment input into the remote command")
	}
}

func TestRunAllowlistedEnvNeverInSSHCommands(t *testing.T) {
	for _, mode := range []string{"argv", "shell", "script", "sync-only"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			logPath := installRecordingSSH(t, dir)
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
			t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
			t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
			const canary = "allowlisted-env-canary-2535"
			t.Setenv("CRABBOX_TEST_VALUE", canary)
			inputPath := filepath.Join(dir, "stdin")
			t.Setenv("CRABBOX_FAKE_SSH_STDIN_LOG", inputPath)
			args := []string{"--provider", "run-env-profile-test", "--no-sync", "--allow-env", "CRABBOX_TEST_VALUE", "--preflight", "--preflight-tools", "none", "--timing-json"}
			proofPath := filepath.Join(dir, "proof.md")
			if mode != "sync-only" {
				args = append(args, "--emit-proof", proofPath)
			}
			switch mode {
			case "shell":
				args = append(args, "--shell", "--", "true")
			case "script":
				script := filepath.Join(dir, "probe.sh")
				mustWriteTestFile(t, script, "#!/bin/sh\ntrue\n")
				args = append(args, "--script", script)
			case "sync-only":
				args = append(args, "--sync-only")
			default:
				args = append(args, "--", "true")
			}
			var stdout, stderr bytes.Buffer
			if err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), args); err != nil {
				t.Fatalf("run error=%v\n%s", err, stderr.String())
			}
			commands, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			for name, value := range map[string]string{"SSH commands": string(commands), "stdout": stdout.String(), "stderr/timing": stderr.String()} {
				if strings.Contains(value, canary) {
					t.Errorf("allowlisted value leaked into %s", name)
				}
			}
			input, err := os.ReadFile(inputPath)
			if err != nil || !bytes.Contains(input, []byte(canary)) {
				t.Errorf("allowlisted value was not delivered over stdin: %v", err)
			}
			if mode != "sync-only" {
				proof, err := os.ReadFile(proofPath)
				if err != nil || bytes.Contains(proof, []byte(canary)) {
					t.Errorf("proof absent or contains env value: %v", err)
				}
			}
		})
	}
}

func TestRunCommandEnvAfterEmptyReplacementList(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX recording SSH fixture")
	}
	for _, value := range []string{"", "append-canary ' \" $literal `literal`\nsecond\rline ☃ "} {
		for _, appendEnv := range []bool{false, true} {
			t.Run(fmt.Sprintf("empty=%t/append=%t", value == "", appendEnv), func(t *testing.T) {
				clearConfigEnv(t)
				dir := t.TempDir()
				t.Chdir(dir)
				isolateRunTestUserDirs(t, dir)
				nativePath := os.Getenv("PATH")
				payloadPath := filepath.Join(dir, "env-upload")
				t.Setenv("CRABBOX_FAKE_ENV_UPLOAD", payloadPath)
				logPath := installRecordingSSH(t, dir, `
case "$match" in
  *'cat > '*'/values.sh'*) /bin/cat > "$CRABBOX_FAKE_ENV_UPLOAD"; exit 0 ;;
esac
`)
				t.Setenv("CRABBOX_CONFIG", "")
				t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
				t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
				t.Setenv("BUILD_FLAVOR", value)
				t.Setenv("CI", "cleared")
				t.Setenv("NODE_OPTIONS", "cleared")
				writeReplacementListConfig(t, "crabbox.yaml", "env:\n  allow: [CI, NODE_OPTIONS, BUILD_FLAVOR]\n")
				writeReplacementListConfig(t, ".crabbox.yaml", "env:\n  allow: []\n")
				args := []string{"--provider", "run-env-profile-test", "--no-sync", "--no-hydrate"}
				if appendEnv {
					args = append(args, "--allow-env", "BUILD_FLAVOR")
				}
				args = append(args, "--", "true")
				var stdout, stderr bytes.Buffer
				if err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(t.Context(), args); err != nil {
					t.Fatalf("run error=%v\n%s", err, stderr.String())
				}
				if !appendEnv {
					if _, err := os.Stat(payloadPath); !os.IsNotExist(err) {
						t.Fatalf("cleared allowlist uploaded an environment: %v", err)
					}
					return
				}
				// Execute the actual upload in a clean child environment so the local
				// BUILD_FLAVOR cannot make a missing or corrupted handoff pass.
				cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", `. "$1"
test "${BUILD_FLAVOR+x}" = x && test "${CI+x}" != x && test "${NODE_OPTIONS+x}" != x || exit 41
printf '%s' "$BUILD_FLAVOR"`, "sh", payloadPath)
				cmd.Env = []string{"PATH=" + nativePath}
				got, err := cmd.CombinedOutput()
				if err != nil || string(got) != value {
					t.Fatalf("appended value did not round trip byte-for-byte: %v", err)
				}
				commands, err := os.ReadFile(logPath)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(commands)+stdout.String()+stderr.String(), "append-canary") {
					t.Fatal("appended value leaked into argv or diagnostics")
				}
			})
		}
	}
}
