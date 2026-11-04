package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

func TestSSHCommandEnvRejectsLostOwnerWithFreshContext(t *testing.T) {
	denied := errors.New("ownership renewal denied")
	owner := &workspaceOwner{renewErr: denied}
	ctx, cancel := context.WithCancel(contextWithWorkspaceOwner(t.Context(), owner))
	cancel()
	ctx = context.WithoutCancel(ctx)
	for _, target := range []SSHTarget{{TargetOS: targetLinux}, {TargetOS: targetMacOS},
		{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, {TargetOS: targetWindows, WindowsMode: windowsModeWSL2}} {
		for _, withInput := range []bool{false, true} {
			var size *int64
			if withInput {
				n := int64(1)
				size = &n
			}
			prepared, err := prepareWorkspaceOwnerRemote(ctx, target, "true", size)
			if !errors.Is(err, denied) || prepared.command != "" {
				t.Fatalf("lost ownership prepared a command: target=%s/%s input=%t err=%v", target.TargetOS, target.WindowsMode, withInput, err)
			}
		}
	}
}

func TestSSHCommandEnvDeliveryAndCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH fixture")
	}
	for _, outcome := range []string{"success", "failure", "cancel", "upload-failure", "large"} {
		t.Run(outcome, func(t *testing.T) {
			isolateTestUserDirs(t)
			dir := t.TempDir()
			workdir := t.TempDir()
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
			if outcome == "large" {
				value += strings.Repeat("long value\n", 8192)
			}
			values := map[string]string{"TEST_VALUE": value, "TEST_EMPTY": "", "INVALID-NAME": "ignored"}
			target := SSHTarget{Host: "fixture.invalid", User: "fixture", Port: "22", TargetOS: targetLinux}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var diagnostics bytes.Buffer
			prepared, err := stageSSHCommandEnv(ctx, target, workdir, values, &diagnostics)
			if outcome == "upload-failure" {
				if err == nil || strings.Contains(err.Error(), canary) {
					t.Fatalf("upload failure was not safely reported: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if outcome == "success" {
					// A failed cleanup or interrupted client can leave this file on
					// a kept lease. Checkpoint archives must still exclude its values.
					mustWriteTestFile(t, filepath.Join(workdir, "result.txt"), "public checkpoint result")
					archive, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", remoteCheckpointArchiveCommand(workdir)).Output()
					if err != nil {
						t.Fatal(err)
					}
					gz, err := gzip.NewReader(bytes.NewReader(archive))
					if err != nil {
						t.Fatal(err)
					}
					gz.Multistream(false) // BSD tar may pad the compressed output.
					defer gz.Close()
					contents, err := io.ReadAll(gz)
					if err != nil {
						t.Fatal(err)
					}
					if bytes.Contains(contents, []byte(canary)) {
						t.Error("checkpoint archive contains the staged command environment")
					}
					if !bytes.Contains(contents, []byte("public checkpoint result")) {
						t.Error("checkpoint archive omitted the ordinary workdir contents")
					}
				}
				info, err := os.Stat(filepath.Join(workdir, prepared.File))
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("private env file permissions: %v, %v", info, err)
				}
				profile := filepath.Join(dir, "actions.env")
				mustWriteTestFile(t, profile, "cd /\n")
				command := remoteCommandWithEnvFiles(workdir, nil, []string{profile, prepared.File}, []string{"/bin/sh", "-c", `test "${TEST_EMPTY+set}" = set && printf '%s' "$TEST_VALUE"`})
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
					if code, _ := runSSHStreamResult(ctx, target, remoteCommandWithEnvFiles(workdir, nil, []string{prepared.File}, []string{"false"}), io.Discard, &diagnostics); code == 0 {
						t.Fatal("workload failure lost")
					}
				}
				if outcome == "cancel" {
					cancel()
				}
				prepared.close()
			}
			if _, err := os.Stat(filepath.Join(workdir, shellDir(prepared.File))); !os.IsNotExist(err) {
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

func TestSSHCommandEnvWindowsCleanupProbe(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell unavailable")
	}
	workdir := t.TempDir()
	path := ".crabbox/env-fixture/values.ps1"
	mustWriteTestFile(t, filepath.Join(workdir, path), "synthetic environment")
	target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	probe := decodePowerShellCommand(t, probeSSHCommandEnvCommand(target, workdir, path))
	for _, state := range []string{"file=present dir=present", "file=absent dir=present", "file=absent dir=absent"} {
		cmd := exec.CommandContext(t.Context(), pwsh, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", probe)
		out, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != state {
			t.Fatalf("Windows cleanup probe: want=%q got=%q err=%v", state, out, err)
		}
		switch state {
		case "file=present dir=present":
			err = os.Remove(filepath.Join(workdir, path))
		case "file=absent dir=present":
			err = os.Remove(filepath.Join(workdir, shellDir(path)))
		}
		if err != nil {
			t.Fatal(err)
		}
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

func TestRunEnvCleanupFailurePreservesWorkloadStatusAndRedactsOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH fixture")
	}
	for _, test := range []struct {
		name       string
		workload   error
		wantCode   int
		wantStatus RunStatus
		wantKind   RunErrorKind
	}{
		{"success", nil, 0, RunStatusSucceeded, RunErrorNone},
		{"workload failure", Exit(23, "workload exited 23"), 23, RunStatusFailed, RunErrorProvider},
		{"timeout", context.DeadlineExceeded, 1, RunStatusTimedOut, RunErrorTimeout},
		{"cancellation", context.Canceled, 1, RunStatusCanceled, RunErrorCanceled},
		{"stored owner timeout", functionalPreflightOwnerError{
			err:   errors.Join(Exit(7, "owner timed out"), context.Canceled),
			cause: context.DeadlineExceeded,
		}, 7, RunStatusTimedOut, RunErrorTimeout},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateTestUserDirs(t)
			dir := t.TempDir()
			workdir := t.TempDir()
			// A nonempty directory is harmless once the forwarded file is gone.
			if test.workload == nil {
				t.Setenv("CRABBOX_ENV_REMOVE_FILE", "1")
			}
			mustWriteTestFile(t, filepath.Join(dir, "ssh"), `#!/bin/sh
for arg do remote=$arg; done
case "$remote" in
 *'rm -f --'*)
   if [ "${CRABBOX_ENV_REMOVE_FILE:-}" = 1 ]; then
     for file in "$CRABBOX_ENV_WORKDIR"/.crabbox/env-*/values.sh; do
       rm -f -- "$file"
       touch "${file%/*}/stray"
     done
   fi
   printf 'secret-cleanup-canary' >&2; exit 9 ;;
 *) /bin/sh -c "$remote" ;;
esac
`)
			if err := os.Chmod(filepath.Join(dir, "ssh"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_ENV_WORKDIR", workdir)
			var diagnostics bytes.Buffer
			prepared, err := stageSSHCommandEnv(t.Context(), SSHTarget{Host: "fixture.invalid", User: "fixture", Port: "22", TargetOS: targetLinux}, workdir, map[string]string{"TEST_VALUE": "secret-cleanup-canary"}, &diagnostics)
			if err != nil {
				t.Fatal(err)
			}
			err = test.workload
			prepared.closeAfter(&err)
			code := 0
			if err != nil {
				code = ExitCodeForError(err, 1)
			}
			if code != test.wantCode {
				t.Fatalf("cleanup result=%v", err)
			}
			if test.workload == nil {
				if err != nil || !strings.Contains(diagnostics.String(), "warning:") || !strings.Contains(diagnostics.String(), shellDir(prepared.File)) {
					t.Fatalf("harmless leftover directory: error=%v diagnostics=%s", err, &diagnostics)
				}
			} else if !strings.Contains(err.Error(), "is still present") || !strings.Contains(err.Error(), prepared.File) {
				t.Fatalf("missing file cleanup diagnostic: %v", err)
			}
			if test.workload != nil && (!errors.Is(err, test.workload) || !strings.HasPrefix(err.Error(), test.workload.Error())) {
				t.Fatalf("workload error lost precedence: %v", err)
			}
			if RunStatusForResult(RunResult{}, err) != test.wantStatus || RunErrorKindForResult(RunResult{}, err) != test.wantKind {
				t.Fatalf("cleanup changed primary classification: %v", err)
			}
			if strings.Contains(fmt.Sprint(err)+diagnostics.String(), "secret-cleanup-canary") {
				t.Fatal("cleanup output leaked")
			}
		})
	}
}

func TestSSHCommandEnvCleanupVerification(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH fixture")
	}
	shells := []string{"/bin/sh"}
	if fish, err := exec.LookPath("fish"); err == nil {
		shells = append(shells, fish)
	}
	for _, loginShell := range shells {
		for _, test := range []struct {
			name, cleanup, probe, wantMessage string
			wantCode                          int
			wantWarning                       bool
		}{
			{name: "removed after failed acknowledgement", cleanup: "removed"},
			{name: "removed after cleanup timeout", cleanup: "timeout"},
			{name: "stray file", cleanup: "stray", wantWarning: true},
			{name: "file remains", wantCode: 7, wantMessage: "is still present"},
			{name: "probe fails", probe: "failure", wantCode: 7, wantMessage: "could not confirm removal"},
			{name: "probe times out", probe: "timeout", wantCode: 7, wantMessage: "could not confirm removal"},
			{name: "probe output invalid", probe: "invalid", wantCode: 7, wantMessage: "could not confirm removal"},
			{name: "directory inaccessible", cleanup: "inaccessible", wantCode: 7, wantMessage: "could not confirm removal"},
		} {
			t.Run(filepath.Base(loginShell)+"/"+test.name, func(t *testing.T) {
				// Exercise real deadline expiry once; the remaining cases also run
				// through Fish to catch accidental login-shell interpretation.
				if loginShell != "/bin/sh" && (test.cleanup == "timeout" || test.probe == "timeout") {
					t.Skip("deadline covered with sh")
				}
				isolateTestUserDirs(t)
				dir, workdir := t.TempDir(), t.TempDir()
				t.Setenv("CRABBOX_ENV_LOGIN_SHELL", loginShell)
				t.Setenv("CRABBOX_ENV_CLEANUP", test.cleanup)
				t.Setenv("CRABBOX_ENV_PROBE", test.probe)
				t.Setenv("CRABBOX_ENV_WORKDIR", workdir)
				t.Setenv("CRABBOX_ENV_PENDING_CLEANUP", filepath.Join(dir, "pending-cleanup"))
				logPath := installRecordingSSH(t, dir, `
case "$match" in
  *'rm -f --'*)
    printf 'secret-cleanup-canary' >&2
    case "$CRABBOX_ENV_CLEANUP" in
      removed) "$CRABBOX_ENV_LOGIN_SHELL" -c "$cmd" || exit $? ;;
      timeout) printf '%s' "$cmd" > "$CRABBOX_ENV_PENDING_CLEANUP" ;;
      stray)
        for file in "$CRABBOX_ENV_WORKDIR"/.crabbox/env-*/values.sh; do
          /bin/rm -f -- "$file"
          /usr/bin/touch "${file%/*}/stray"
        done ;;
      inaccessible) /bin/chmod 000 "$CRABBOX_ENV_WORKDIR"/.crabbox/env-* ;;
    esac
    if [ "$CRABBOX_ENV_CLEANUP" = timeout ]; then exec /bin/sleep 60; fi
    exit 9 ;;
  *'file=absent'*|*'file=present'*)
    printf 'secret-probe-canary' >&2
    if [ "$CRABBOX_ENV_CLEANUP" = timeout ]; then
      # Simulate remote cleanup completing after the client timed out.
      pending=$(/bin/cat "$CRABBOX_ENV_PENDING_CLEANUP")
      "$CRABBOX_ENV_LOGIN_SHELL" -c "$pending" || exit $?
    fi
    case "$CRABBOX_ENV_PROBE" in
      failure) exit 9 ;;
      timeout) exec /bin/sleep 60 ;;
      invalid) printf 'secret-probe-canary'; exit 0 ;;
    esac ;;
esac
exec "$CRABBOX_ENV_LOGIN_SHELL" -c "$cmd"
`)
				var diagnostics bytes.Buffer
				ctx, cancel := context.WithCancel(t.Context())
				prepared, err := stageSSHCommandEnv(ctx, SSHTarget{Host: "fixture.invalid", User: "fixture", Port: "22", TargetOS: targetLinux}, workdir, map[string]string{"TEST_VALUE": "secret-cleanup-canary"}, &diagnostics)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(filepath.Join(workdir, shellDir(prepared.File)), 0700) })
				cancel() // Cleanup and verification must each get their own budget.
				started := time.Now()
				prepared.closeAfter(&err)
				elapsed := time.Since(started)
				if ExitCodeForError(err, 0) != test.wantCode || (test.wantMessage != "" && (err == nil || !strings.Contains(err.Error(), test.wantMessage) || !strings.Contains(err.Error(), prepared.File))) {
					t.Fatalf("cleanup result=%v, want code=%d message=%q", err, test.wantCode, test.wantMessage)
				}
				if strings.Contains(diagnostics.String(), "warning:") != test.wantWarning || (test.wantWarning && !strings.Contains(diagnostics.String(), shellDir(prepared.File))) {
					t.Fatalf("cleanup diagnostics=%q", diagnostics.String())
				}
				if !test.wantWarning && diagnostics.Len() != 0 {
					t.Fatalf("unexpected cleanup diagnostics: %s", &diagnostics)
				}
				if strings.Contains(fmt.Sprint(err)+diagnostics.String(), "canary") {
					t.Fatal("cleanup/probe output leaked")
				}
				minimum := 9 * time.Second
				if test.cleanup == "timeout" {
					minimum = 14 * time.Second
				}
				if (test.cleanup == "timeout" || test.probe == "timeout") && (elapsed < minimum || elapsed > 35*time.Second) {
					t.Fatalf("cleanup/verification deadline not enforced: %v", elapsed)
				}
				commands, err := os.ReadFile(logPath)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Count(string(commands), "rm -f --") != 1 || strings.Count(string(commands), "file=present") != 1 {
					t.Fatalf("expected one cleanup and one verification probe: %s", commands)
				}
			})
		}
	}
}

func TestSSHCommandEnvCleanupVerificationNative(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("executes the real Linux command runtime")
	}
	binary := filepath.Join(t.TempDir(), "crabbox-runtime")
	build := exec.Command("go", "build", "-trimpath", "-o", binary, "./cmd/crabbox-runtime")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build command runtime: %v\n%s", err, output)
	}
	for _, lostAcknowledgement := range []bool{false, true} {
		t.Run(fmt.Sprintf("lost-ack=%t", lostAcknowledgement), func(t *testing.T) {
			isolateTestUserDirs(t)
			home, workdir, tools := t.TempDir(), t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CRABBOX_ENV_WORKDIR", workdir)
			t.Setenv("CRABBOX_ENV_EFFECTS", filepath.Join(tools, "effects"))
			t.Setenv("CRABBOX_ENV_LOST_ACK", fmt.Sprint(lostAcknowledgement))
			path := os.Getenv("PATH")
			installRecordingSSH(t, tools, `
before=0
for file in "$CRABBOX_ENV_WORKDIR"/.crabbox/env-*/values.sh; do
  if [ -f "$file" ]; then before=1; fi
done
output="$CRABBOX_ENV_EFFECTS.stdout.$$"
trap '/bin/rm -f "$output"' EXIT
/bin/sh -c "$cmd" > "$output"
code=$?
/bin/cat "$output"
after=0
for file in "$CRABBOX_ENV_WORKDIR"/.crabbox/env-*/values.sh; do
  if [ -f "$file" ]; then after=1; fi
done
if [ "$before" = 1 ] && [ "$after" = 0 ]; then
  printf 'removed\n' >> "$CRABBOX_ENV_EFFECTS"
  if [ "$CRABBOX_ENV_LOST_ACK" = true ]; then exit 9; fi
fi
if [ "$(/bin/cat "$output")" = 'file=absent dir=absent' ]; then
  printf 'probe\n' >> "$CRABBOX_ENV_EFFECTS"
fi
exit "$code"
`)
			t.Setenv("PATH", tools+string(os.PathListSeparator)+path)
			target := SSHTarget{Host: "fixture.invalid", User: "fixture", Port: "22", TargetOS: targetLinux, NoControlMaster: true}
			owner, err := acquireWorkspaceOwnerWithTransport(t.Context(), target, "cbx_env_cleanup", io.Discard, localPOSIXWorkspaceOwnerTransport{home: home}, workspaceOwnerWaitTimeout, workspaceOwnerTTL, workspaceOwnerRenewInterval)
			if err != nil {
				t.Fatal(err)
			}
			ownerClosed := false
			t.Cleanup(func() {
				if ownerClosed {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), workspaceOwnerCleanupTimeout)
				defer cancel()
				if err := owner.Close(ctx); err != nil {
					t.Errorf("release workspace owner: %v", err)
				}
			})
			runtimePath := filepath.Join(tools, "runtime")
			nonceLog := filepath.Join(tools, "nonces")
			launcher := filepath.Join(tools, "runtime-launcher")
			writeExecutable(t, launcher, "#!/bin/sh\nif [ \"$1\" = __remote-control ] && [ \"$2\" = run ]; then printf '%s\\n' \"$4\" >> "+shellQuote(nonceLog)+"; fi\nexec "+shellQuote(binary)+" \"$@\"\n")
			if err := os.Symlink(launcher, runtimePath); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				data, err := os.ReadFile(nonceLog)
				if os.IsNotExist(err) {
					return
				}
				if err != nil {
					t.Errorf("read owned runtime nonces: %v", err)
					return
				}
				for _, nonce := range strings.Fields(string(data)) {
					if len(nonce) != 32 || strings.Trim(nonce, "0123456789abcdef") != "" {
						t.Error("invalid owned runtime nonce")
						continue
					}
					stage := "/tmp/crabbox-command-" + nonce
					if _, statErr := os.Lstat(stage); os.IsNotExist(statErr) {
						continue
					}
					if !t.Failed() {
						t.Error("owned runtime stage was not retired")
					}
					ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
					cmd := exec.CommandContext(ctx, binary, "__remote-control", "control", "CBX-REMOTE-1", nonce, "cleanup", "10000")
					cleanupErr := cmd.Run()
					if cleanupErr == nil {
						if _, statErr := os.Lstat(stage); statErr == nil {
							cleanupErr = exec.CommandContext(ctx, binary, "__remote-control", "control", "CBX-REMOTE-1", nonce, "retire").Run()
						}
					}
					cancel()
					if cleanupErr != nil {
						t.Errorf("settle owned runtime stage: %v", cleanupErr)
					}
					if _, statErr := os.Lstat(stage); !os.IsNotExist(statErr) {
						t.Errorf("owned runtime stage remains: %v", statErr)
					}
				}
			})
			scope := testNativeRuntimeScope()
			scope.manifest = "selected fixture"
			scope.install = func(_ context.Context, selected SSHTarget, _ runtimeartifact.Source) (*remoteNativeRuntime, error) {
				return &remoteNativeRuntime{target: selected, path: runtimePath}, nil
			}
			ctx := context.WithValue(runtimeLeaseContext(contextWithWorkspaceOwner(owner.Context(), owner), "cbx_env_cleanup"), nativeRuntimeScopeKey{}, scope)
			ctx, err = scope.admit(ctx, target)
			if err != nil {
				t.Fatal(err)
			}
			var diagnostics bytes.Buffer
			prepared, err := stageSSHCommandEnv(ctx, target, workdir, map[string]string{"TEST_VALUE": "cleanup-private-value"}, &diagnostics)
			if err != nil {
				t.Fatalf("native environment upload: %v", err)
			}
			file := filepath.Join(workdir, prepared.File)
			if data, err := os.ReadFile(file); err != nil || !bytes.Contains(data, []byte("cleanup-private-value")) {
				t.Fatalf("environment upload missing: %v", err)
			}
			// Finite cleanup must retain workspace authority without depending on
			// another foreground runtime invocation after the workload finishes.
			if err := os.Remove(runtimePath); err != nil {
				t.Fatal(err)
			}
			prepared.closeAfter(&err)
			if err != nil || diagnostics.Len() != 0 {
				t.Fatalf("environment cleanup: %v; diagnostics=%q", err, &diagnostics)
			}
			if _, err := os.Stat(filepath.Dir(file)); !os.IsNotExist(err) {
				t.Fatalf("environment directory remains: %v", err)
			}
			effects, err := os.ReadFile(filepath.Join(tools, "effects"))
			want := "removed\n"
			if lostAcknowledgement {
				want += "probe\n"
			}
			if err != nil || string(effects) != want {
				t.Fatalf("cleanup effects=%q, want %q; error=%v", effects, want, err)
			}
			if err := owner.Close(t.Context()); err != nil {
				t.Fatalf("workspace owner did not close: %v", err)
			}
			ownerClosed = true
			if err := scope.finish(t.Context()); err != nil {
				t.Fatalf("native command stage retained: %v", err)
			}
		})
	}
}

func TestRunEnvCleanupCancellationKeepsPrimaryOutcome(t *testing.T) {
	for _, test := range []struct {
		name       string
		primary    error
		cleanup    error
		result     RunResult
		wantStatus RunStatus
		wantKind   RunErrorKind
		wantCode   int
	}{
		{"timeout then canceled cleanup", errors.Join(Exit(7, "owner timed out"), context.DeadlineExceeded), context.Canceled, RunResult{}, RunStatusTimedOut, RunErrorTimeout, 7},
		{"provider failure then cleanup timeout", errors.New("owner failed"), context.DeadlineExceeded, RunResult{}, RunStatusFailed, RunErrorProvider, 1},
		{"workload failure then canceled cleanup", Exit(23, "workload failed"), context.Canceled, RunResult{ExitCode: 23}, RunStatusFailed, RunErrorCommandExit, 23},
	} {
		t.Run(test.name, func(t *testing.T) {
			cleanupErr := errors.Join(Exit(7, "synthetic cleanup failure"), test.cleanup)
			prepared := sshCommandEnv{close: func() error { return cleanupErr }}
			err := test.primary
			prepared.closeAfter(&err)
			if !errors.Is(err, test.primary) || !strings.Contains(err.Error(), cleanupErr.Error()) {
				t.Fatalf("missing primary error or cleanup diagnostic: %v", err)
			}
			if RunStatusForResult(test.result, err) != test.wantStatus || RunErrorKindForResult(test.result, err) != test.wantKind || ExitCodeForError(err, 1) != test.wantCode {
				t.Fatalf("cleanup changed primary outcome: %v", err)
			}
		})
	}
}
