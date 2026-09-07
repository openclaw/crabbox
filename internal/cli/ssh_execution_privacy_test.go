package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSSHExecutionKeepsManagedIdentityPrivate(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX local SSH fixture")
	}
	for _, operation := range []string{"command", "input", "control", "fallback", "screenshot", "checkpoint", "wsl-probe", "failure", "cancel"} {
		t.Run(operation, func(t *testing.T) {
			isolateTestUserDirs(t)
			dir := t.TempDir()
			t.Setenv("CRABBOX_EXECUTION_FIXTURE", dir)
			t.Setenv("CRABBOX_EXECUTION_MODE", operation)
			script := `#!/bin/sh
count=0
[ ! -f "$CRABBOX_EXECUTION_FIXTURE/count" ] || read count < "$CRABBOX_EXECUTION_FIXTURE/count"
count=$((count + 1))
printf '%s\n' "$count" > "$CRABBOX_EXECUTION_FIXTURE/count"
printf '%s\n' "$@" > "$CRABBOX_EXECUTION_FIXTURE/$count.argv"
config=
previous=
port=
for arg do
  [ "$previous" != -F ] || config=$arg
  [ "$previous" != -p ] || port=$arg
  previous=$arg
done
if [ -n "$config" ]; then
  cp "$config" "$CRABBOX_EXECUTION_FIXTURE/$count.config"
  printf '%s\n' "$config" > "$CRABBOX_EXECUTION_FIXTURE/$count.path"
  case "$(uname -s)" in
    Darwin) mode=$(stat -f %Lp "$config") ;;
    *) mode=$(stat -c %a "$config") ;;
  esac
  [ "$mode" = 600 ] || exit 81
  if grep -q 'Port "2222"' "$config"; then port=2222; fi
fi
[ "$port" != 2222 ] || exit 255
case "$CRABBOX_EXECUTION_MODE" in
  failure) exit 23 ;;
  cancel) touch "$CRABBOX_EXECUTION_FIXTURE/started"; exec sleep 60 ;;
esac
printf execution-ok
cat
`
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			target := SSHTarget{User: "synthetic-execution-credential", Host: "fixture.invalid", Port: "22", AuthSecret: true, TargetOS: targetLinux}
			var stdout bytes.Buffer
			var err error
			switch operation {
			case "command":
				_, err = runSSHStreamResult(t.Context(), target, "true", &stdout, io.Discard)
			case "input":
				err = runSSHInput(t.Context(), target, "cat", strings.NewReader("private-script-input"), &stdout, io.Discard)
			case "control":
				err = executePreparedSSH(t.Context(), &target, "true", nil, 0, sshCommandLimit{}, "10", "3", &stdout, io.Discard)
			case "fallback":
				target.Port, target.FallbackPorts = "2222", []string{"22"}
				err = runSSHQuietWithOptionsResolvePort(t.Context(), &target, "true", "10", "3")
			case "screenshot":
				err = runSSHToWriter(t.Context(), target, "true", &stdout)
			case "checkpoint":
				_, err = createCheckpointArchive(t.Context(), target, "/workspace", filepath.Join(dir, "archive.tgz"))
			case "wsl-probe":
				err = probeWSLStageTransport(t.Context(), target, "10", "3")
			case "failure":
				_, err = runSSHStreamResult(t.Context(), target, "exit 23", &stdout, io.Discard)
			case "cancel":
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				joined := false
				t.Cleanup(func() {
					cancel()
					if !joined {
						<-done
					}
				})
				go func() { _, runErr := runSSHStreamResult(ctx, target, "sleep 60", &stdout, io.Discard); done <- runErr }()
				deadline := time.Now().Add(5 * time.Second)
				for {
					if _, statErr := os.Stat(filepath.Join(dir, "started")); statErr == nil {
						break
					}
					select {
					case earlyErr := <-done:
						joined = true
						t.Fatalf("execution stopped before cancellation: %v", earlyErr)
					case <-time.After(10 * time.Millisecond):
					}
					if time.Now().After(deadline) {
						t.Fatal("SSH did not start")
					}
				}
				cancel()
				err = <-done
				joined = true
			}
			if operation == "cancel" || operation == "failure" {
				if err == nil {
					t.Fatal("execution failure was suppressed")
				}
				if operation == "failure" && exitCode(err) != 23 {
					t.Fatalf("exit=%d", exitCode(err))
				}
			} else if err != nil {
				t.Fatalf("execution failed: %v", err)
			}
			if operation == "input" && stdout.String() != "execution-okprivate-script-input" {
				t.Fatalf("input delivery=%q", stdout.String())
			}
			if operation == "fallback" && target.Port != "22" {
				t.Fatalf("resolved port=%s", target.Port)
			}
			calls, err := filepath.Glob(filepath.Join(dir, "*.argv"))
			if err != nil || len(calls) == 0 {
				t.Fatalf("missing SSH invocation: %v", err)
			}
			for _, call := range calls {
				args, err := os.ReadFile(call)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(args, []byte(target.User)) {
					t.Fatal("managed SSH credential was exposed in local process argv")
				}
				if !bytes.Contains(args, []byte(sshTransportHostAlias)) {
					t.Fatal("SSH command did not select the private transport config")
				}
				stem := strings.TrimSuffix(call, ".argv")
				config, err := os.ReadFile(stem + ".config")
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{`User "` + target.User + `"`, `IdentityFile "none"`, `IdentityAgent "none"`, "ControlMaster no"} {
					if !bytes.Contains(config, []byte(want)) {
						t.Fatalf("private config omitted %s", strings.Fields(want)[0])
					}
				}
				configPath, err := os.ReadFile(stem + ".path")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(strings.TrimSpace(string(configPath))); !os.IsNotExist(err) {
					t.Fatal("private config survived completed execution")
				}
			}
		})
	}
}
