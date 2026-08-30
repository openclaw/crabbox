//go:build !windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	for _, transport := range []string{"posix", "staged-wsl"} {
		t.Run(transport, func(t *testing.T) {
			for _, scenario := range []string{"result", "legacy-stream", "nil-stderr", "writer-error", "short-write", "success", "workload-74"} {
				t.Run(scenario, func(t *testing.T) {
					home, owner := workspaceOwnerSetupFixture(t)
					denied := scenario != "success" && scenario != "workload-74"
					path := os.Getenv("PATH")
					if denied {
						path = workspaceOwnerDenySignals(t)
					}
					target := SSHTarget{Host: "127.0.0.1", User: "runner", Port: "22", FallbackPorts: []string{}, TargetOS: targetLinux, NoControlMaster: true}
					var stage *workspaceOwnerWSLStageFixture
					if transport == "staged-wsl" {
						target.TargetOS, target.WindowsMode = targetWindows, windowsModeWSL2
						stage = installWorkspaceOwnerWSLStageFixture(t, home, path)
					} else {
						tools := t.TempDir()
						writeExecutable(t, filepath.Join(tools, "ssh"), "#!/bin/sh\nfor arg; do remote=\"$arg\"; done\nHOME="+shellQuote(home)+"\nPATH="+shellQuote(path)+"\nexport HOME PATH\nexec /bin/sh -c \"$remote\"\n")
						t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
					}
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
					if stage != nil {
						stage.requireCalls(t, 1, 1, map[bool]int{true: 1, false: 0}[code != 0])
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
