//go:build !windows

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type workspaceOwnerDiagnosticSink struct {
	writes int
	short  bool
	err    error
}

func (w *workspaceOwnerDiagnosticSink) Write(data []byte) (int, error) {
	w.writes++
	if w.short {
		return len(data) - 1, nil
	}
	return 0, w.err
}

func TestWorkspaceOwnerStreamingSetupDiagnostic(t *testing.T) {
	for _, scenario := range []string{"result", "legacy-stream", "nil-stderr", "writer-error", "short-write", "success", "workload-74"} {
		t.Run(scenario, func(t *testing.T) {
			home, owner := workspaceOwnerSetupFixture(t)
			denied := scenario != "success" && scenario != "workload-74"
			path := os.Getenv("PATH")
			if denied {
				path = workspaceOwnerDenySignals(t)
			}
			tools := t.TempDir()
			writeExecutable(t, filepath.Join(tools, "ssh"), "#!/bin/sh\nfor arg; do remote=\"$arg\"; done\nHOME="+shellQuote(home)+"\nPATH="+shellQuote(path)+"\nexport HOME PATH\nexec /bin/sh -c \"$remote\"\n")
			t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			target := SSHTarget{Host: "127.0.0.1", User: "runner", Port: "22", FallbackPorts: []string{}, TargetOS: targetLinux, NoControlMaster: true}
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			ctx = contextWithWorkspaceOwner(ctx, owner)
			marker := filepath.Join(home, "workload-started")
			const userStderr = "CRABBOX_OWNER_SETUP_V1 workload-output failed registration\n"
			remote := "touch " + shellQuote(marker) + "; printf stdout; printf %s " + shellQuote(userStderr)
			remote += " >&2"
			if scenario == "workload-74" {
				remote += "; exit 74"
			}
			var stdout, stderr bytes.Buffer
			var destination io.Writer = &stderr
			writeFailure := errors.New("diagnostic writer unavailable")
			sink := &workspaceOwnerDiagnosticSink{err: writeFailure}
			switch scenario {
			case "nil-stderr":
				destination = nil
			case "writer-error":
				destination = sink
			case "short-write":
				sink.short = true
				destination = sink
			}
			var code int
			var err error
			if scenario == "legacy-stream" {
				code = runSSHStream(ctx, target, remote, &stdout, destination)
			} else {
				code, err = runSSHStreamResult(ctx, target, remote, &stdout, destination)
			}
			if ctx.Err() != nil {
				t.Fatal("streaming witness exceeded its setup bound")
			}
			var setupErr *workspaceOwnerSetupError
			if !denied {
				wantCode := 0
				if scenario == "workload-74" {
					wantCode = 74
				}
				if code != wantCode || errors.As(err, &setupErr) || stdout.String() != "stdout" || stderr.String() != userStderr {
					t.Fatalf("streaming workload behavior changed: code=%d typed-setup=%t", code, setupErr != nil)
				}
				return
			}
			if code != 74 || stdout.Len() != 0 {
				t.Fatalf("streaming setup failure changed exit/output: code=%d stdout-bytes=%d", code, stdout.Len())
			}
			if scenario != "legacy-stream" && (!errors.As(err, &setupErr) || setupErr.phase != "registration") {
				t.Fatal("streaming setup failure lost its typed error")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("streaming setup failure executed the workload")
			}
			switch scenario {
			case "writer-error":
				if !errors.Is(err, writeFailure) || sink.writes != 1 {
					t.Fatal("streaming diagnostic lost the writer error or wrote more than once")
				}
			case "short-write":
				if !errors.Is(err, io.ErrShortWrite) || sink.writes != 1 {
					t.Fatal("streaming diagnostic lost the short write or wrote more than once")
				}
			case "nil-stderr":
				if stderr.Len() != 0 {
					t.Fatal("nil stderr received a diagnostic")
				}
			default:
				want := (&workspaceOwnerSetupError{phase: "registration"}).Error() + "\n"
				if stderr.String() != want {
					t.Fatalf("streaming setup needs exactly one safe diagnostic: got %d bytes, want %d", stderr.Len(), len(want))
				}
			}
		})
	}
}

// This fake SSH validates the WSL command without logging arguments, then runs
// the actual stdin script under a local POSIX shell. Only frame digests persist.
func TestWorkspaceOwnerWSLControlSSHProcess(t *testing.T) {
	if os.Getenv("CRABBOX_TEST_WSL_CONTROL_SSH") != "1" {
		return
	}
	os.Exit(workspaceOwnerWSLControlSSHProcess())
}

func workspaceOwnerWSLControlSSHProcess() int {
	args := os.Args
	port := ""
	for i, arg := range args {
		if arg == "-E" {
			return 91 // Control calls must not enable local mux diagnostics/retries.
		}
		if arg == "-p" && i+1 < len(args) {
			port = args[i+1]
		}
	}
	frame, err := io.ReadAll(io.LimitReader(os.Stdin, 1024*1024))
	if err != nil || len(frame) == 0 || len(frame) >= 1024*1024 {
		return 92
	}
	if args[len(args)-1] != wsl2StdinScriptCommandWithWaitTimeout(len(frame), 15*time.Second) {
		return 93
	}
	log, err := os.OpenFile(os.Getenv("CRABBOX_TEST_WSL_CONTROL_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 94
	}
	_, writeErr := fmt.Fprintf(log, "%s %x %d\n", port, sha256.Sum256(frame), len(frame))
	if err := log.Close(); err != nil || writeErr != nil {
		return 94
	}
	if port == "2222" && os.Getenv("CRABBOX_TEST_WSL_CONTROL_DENIED") != "1" {
		fmt.Fprint(os.Stderr, "mm_send_fd: sendmsg(1): broken pipe\n"+sshMuxDescriptorFailure+"\n")
		return 255
	}
	cmd := exec.Command("/bin/sh", "-s")
	cmd.Env = []string{"HOME=" + os.Getenv("CRABBOX_TEST_WSL_CONTROL_HOME"), "PATH=" + os.Getenv("CRABBOX_TEST_WSL_CONTROL_PATH")}
	cmd.Stdin = bytes.NewReader(frame)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return exitCode(cmd.Run())
}

func TestWorkspaceOwnerWSLControlSetupDiagnostic(t *testing.T) {
	for _, scenario := range []string{"setup-denied", "success", "workload-74", "ownerless"} {
		t.Run(scenario, func(t *testing.T) {
			home, owner := workspaceOwnerSetupFixture(t)
			path := os.Getenv("PATH")
			denied := scenario == "setup-denied"
			if denied {
				path = workspaceOwnerDenySignals(t)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			tools := t.TempDir()
			writeExecutable(t, filepath.Join(tools, "ssh"), "#!/bin/sh\nexec "+shellQuote(executable)+" -test.run='^TestWorkspaceOwnerWSLControlSSHProcess$' -- \"$@\"\n")
			t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_TEST_WSL_CONTROL_SSH", "1")
			t.Setenv("CRABBOX_TEST_WSL_CONTROL_HOME", home)
			t.Setenv("CRABBOX_TEST_WSL_CONTROL_PATH", path)
			t.Setenv("CRABBOX_TEST_WSL_CONTROL_DENIED", map[bool]string{true: "1", false: "0"}[denied])
			logPath := filepath.Join(tools, "frames")
			t.Setenv("CRABBOX_TEST_WSL_CONTROL_LOG", logPath)
			target := SSHTarget{Host: "127.0.0.1", User: "runner", Port: "2222", FallbackPorts: []string{"22"}, TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			if scenario != "ownerless" {
				ctx = contextWithWorkspaceOwner(ctx, owner)
			}
			marker := filepath.Join(home, "workload-started")
			remote := "touch " + shellQuote(marker) + "; printf control-output"
			if scenario == "workload-74" {
				remote += "; exit 74"
			}
			out, err := runWSL2ControlScriptCombinedOutput(ctx, target, remote, 15*time.Second, "2", "1")
			if ctx.Err() != nil {
				t.Fatal("local WSL control fixture exceeded its bound")
			}
			var setupErr *workspaceOwnerSetupError
			if denied {
				if !errors.As(err, &setupErr) || setupErr.phase != "registration" || out != "" {
					t.Fatalf("WSL setup lost typed error or leaked marker: typed=%t output-bytes=%d", setupErr != nil, len(out))
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("WSL setup failure executed workload")
				}
			} else {
				wantCode := 0
				if scenario == "workload-74" {
					wantCode = 74
				}
				if exitCode(err) != wantCode || errors.As(err, &setupErr) || out != "control-output" {
					t.Fatalf("WSL workload behavior changed: exit=%d typed=%t output-bytes=%d", exitCode(err), setupErr != nil, len(out))
				}
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if denied {
				if len(lines) != 1 || !strings.HasPrefix(lines[0], "2222 ") {
					t.Fatal("WSL setup failure retried a port")
				}
			} else if len(lines) != 2 || !strings.HasPrefix(lines[0], "2222 ") || !strings.HasPrefix(lines[1], "22 ") || strings.TrimPrefix(lines[0], "2222 ") != strings.TrimPrefix(lines[1], "22 ") {
				t.Fatal("WSL fallback changed frame bytes or introduced mux retries")
			}
		})
	}
}

func TestWorkspaceOwnerDashSignalHandoff(t *testing.T) {
	const shell = "/bin/dash"
	if _, err := os.Stat(shell); err != nil {
		t.Skip("optional local dash is unavailable")
	}
	for _, signal := range []string{"INT", "QUIT", "PIPE"} {
		for _, ignored := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/inherited-ignore-%t", signal, ignored), func(t *testing.T) {
				home, owner := workspaceOwnerSetupFixture(t)
				payload := "exec " + shell + " -c " + shellQuote("trap 'exit 42' "+signal+"; kill -"+signal+" $$; printf survived-signal")
				script := remoteWorkspaceOwnerPOSIXWitnessScript(owner.key, owner.token, payload, "")
				// Route only this generated fixture's shells to dash; no global
				// shell replacement and no changes to the production launcher.
				script = strings.ReplaceAll(script, "/bin/sh", shell)
				script = strings.ReplaceAll(script, "exec sh -c", "exec "+shell+" -c")
				prefix := "ulimit -c 0; "
				if ignored {
					prefix += "trap '' " + signal + "; "
				}
				cmd, ctx := boundedWorkspaceOwnerCommand(t, home, os.Getenv("PATH"), prefix+"exec "+shell+" -c "+shellQuote(script))
				cmd.Path, cmd.Args[0] = shell, shell
				out, err := cmd.CombinedOutput()
				if ctx.Err() != nil {
					t.Fatal("dash witness exceeded its bound")
				}
				if ignored {
					if err != nil || string(out) != "survived-signal" {
						t.Fatalf("dash changed inherited signal ignore: exit=%d output-bytes=%d", exitCode(err), len(out))
					}
				} else if exitCode(err) != 42 || len(out) != 0 {
					t.Fatalf("dash changed caught/default signal handoff: exit=%d output-bytes=%d", exitCode(err), len(out))
				}
				req := workspaceOwnerRemoteRequest{Action: workspaceOwnerRelease, Key: owner.key, Token: owner.token}
				if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(req)); err != nil || out != "RELEASED" {
					t.Fatal("dash signal exit retained a child witness")
				}
			})
		}
	}
}
