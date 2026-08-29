package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceOwnerPOSIXWitnessSignalDispositions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX witness requires a POSIX shell")
	}
	for _, signal := range []string{"INT", "QUIT"} {
		for _, ignored := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/inherited-ignore-%t", signal, ignored), func(t *testing.T) {
				home := t.TempDir()
				key := workspaceOwnerKey("cbx_signal_disposition")
				token := strings.Repeat("a", 64)
				request := workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: key, Token: token, TTL: time.Minute}
				if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request)); err != nil || out != "ACQUIRED" {
					t.Fatalf("acquire out=%q err=%v", out, err)
				}
				// A fresh shell may ignore QUIT itself, but can trap it unless it
				// inherited SIG_IGN. This tests inheritance across shell variants.
				payload := "exec /bin/sh -c " + shellQuote("trap 'exit 42' "+signal+"; kill -"+signal+" $$; printf survived-signal")
				prefix := "ulimit -c 0; "
				if ignored {
					prefix += "trap '' INT QUIT; "
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "/bin/sh", "-c", prefix+remoteWorkspaceOwnerPOSIXWitness(key, token, payload))
				cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
				out, err := cmd.CombinedOutput()
				if ctx.Err() != nil {
					t.Fatalf("witness timed out: %s", out)
				}
				if ignored {
					if err != nil || string(out) != "survived-signal" {
						t.Errorf("intentional inherited ignore changed: out=%q err=%v", out, err)
					}
				} else if err == nil || exitCode(err) != 42 || strings.Contains(string(out), "survived-signal") {
					t.Errorf("witness ignored default signal: out=%q err=%v", out, err)
				}
				ownerRoot := filepath.Join(home, ".crabbox", "workspace-owners")
				entries, err := os.ReadDir(ownerRoot)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if strings.Contains(entry.Name(), ".run.") || strings.Contains(entry.Name(), ".launcher.") || strings.HasSuffix(entry.Name(), ".child") {
						t.Errorf("finished signal witness left control state %q", entry.Name())
					}
				}
				if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIXWitness(key, token, "printf next-command")); err != nil || out != "next-command" {
					t.Errorf("next command after signal: out=%q err=%v", out, err)
				}
			})
		}
	}
}

func TestWorkspaceOwnerPOSIXWitnessInputAndAuthority(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX witness requires a POSIX shell")
	}
	for _, scenario := range []string{"binary-input", "replaced-live-record", "failed-terminal-observation", "overlapping-stage"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			key := workspaceOwnerKey("cbx_signal_authority")
			token := strings.Repeat("b", 64)
			request := workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: key, Token: token, TTL: time.Minute}
			if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request)); err != nil || out != "ACQUIRED" {
				t.Fatalf("acquire out=%q err=%v", out, err)
			}
			root := filepath.Join(home, ".crabbox", "workspace-owners")
			childPath := filepath.Join(root, key+".child")
			input := bytes.Repeat([]byte("binary\x00input\xff\n"), 32_768)
			payload := `if (printf unexpected >&3) 2>/dev/null; then exit 98; fi
if (printf unexpected >&4) 2>/dev/null; then exit 98; fi
cat; printf stderr-ok >&2`
			path := os.Getenv("PATH")
			var preservedRecord []byte
			switch scenario {
			case "replaced-live-record":
				identity, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(os.Getpid())).Output()
				if err != nil {
					t.Fatal(err)
				}
				preservedRecord = []byte(fmt.Sprintf("%d\n%s\n", os.Getpid(), strings.Join(strings.Fields(string(identity)), " ")))
				payload = "printf %s " + shellQuote(string(preservedRecord)) + " > " + shellQuote(childPath)
			case "failed-terminal-observation":
				marker := filepath.Join(home, "payload-finished")
				realPS, err := exec.LookPath("ps")
				if err != nil {
					t.Fatal(err)
				}
				tools := filepath.Join(home, "tools")
				if err := os.Mkdir(tools, 0o700); err != nil {
					t.Fatal(err)
				}
				probe := "#!/bin/sh\n[ ! -f " + shellQuote(marker) + " ] || exit 1\nexec " + shellQuote(realPS) + " \"$@\"\n"
				if err := os.WriteFile(filepath.Join(tools, "ps"), []byte(probe), 0o700); err != nil {
					t.Fatal(err)
				}
				path = tools + string(os.PathListSeparator) + path
				payload = "touch " + shellQuote(marker)
			case "overlapping-stage":
				payload = "touch " + shellQuote(filepath.Join(home, "ready")) + "; attempts=0; while [ ! -f " + shellQuote(filepath.Join(home, "continue")) + " ]; do attempts=$((attempts+1)); [ \"$attempts\" -lt 200 ] || exit 124; sleep 0.05; done; " +
					"cat " + shellQuote(root) + "/*.run.*/input; printf stderr-ok >&2"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", remoteWorkspaceOwnerPOSIXWitness(key, token, payload, true))
			cmd.Env = []string{"HOME=" + home, "PATH=" + path}
			cmd.Stdin = bytes.NewReader(input)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if scenario == "overlapping-stage" {
				deadline := time.Now().Add(10 * time.Second)
				for {
					if _, err := os.Stat(filepath.Join(home, "ready")); err == nil {
						break
					}
					if time.Now().After(deadline) {
						cancel()
						_ = cmd.Wait()
						t.Fatal("first witness did not reach payload")
					}
					time.Sleep(10 * time.Millisecond)
				}
				if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIXWitness(key, token, "printf unexpected-overlap")); err == nil || strings.Contains(out, "unexpected-overlap") {
					t.Errorf("concurrent witness entered live workspace: out=%q err=%v", out, err)
				}
				if err := os.WriteFile(filepath.Join(home, "continue"), nil, 0o600); err != nil {
					cancel()
					_ = cmd.Wait()
					t.Fatal(err)
				}
			}
			err := cmd.Wait()
			if scenario == "binary-input" || scenario == "overlapping-stage" {
				if err != nil || !bytes.Equal(stdout.Bytes(), input) || stderr.String() != "stderr-ok" {
					t.Fatalf("stream changed: stdout=%d bytes stderr=%q err=%v", stdout.Len(), stderr.String(), err)
				}
				if _, err := os.Stat(childPath); !os.IsNotExist(err) {
					t.Errorf("completed witness retained child: %v", err)
				}
			} else {
				if err == nil || exitCode(err) != 74 {
					t.Errorf("ambiguous cleanup did not fail closed: err=%v stderr=%q", err, stderr.String())
				}
				data, err := os.ReadFile(childPath)
				if err != nil || len(data) == 0 {
					t.Fatalf("ambiguous cleanup removed child record: err=%v", err)
				}
				if preservedRecord != nil && !bytes.Equal(data, preservedRecord) {
					t.Errorf("replacement live record changed: got=%q want=%q", data, preservedRecord)
				}
			}
		})
	}
}
