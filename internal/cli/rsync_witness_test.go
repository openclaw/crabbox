//go:build !windows

package cli

import (
	"bytes"
	"context"
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
)

type rsyncWitnessTestTransport struct {
	workspaceOwnerTransportFunc
}

func (rsyncWitnessTestTransport) CallBudget() time.Duration { return 20 * time.Millisecond }

func TestRsyncWitnessRetainsStopUntilQuiescent(t *testing.T) {
	for _, response := range []string{"CHILD", "AMBIGUOUS", "OWNED"} {
		t.Run(response, func(t *testing.T) {
			home, bin := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			writeExecutable(t, filepath.Join(bin, "ssh"), "#!/bin/sh\nfor arg do remote=$arg; done\nexec /bin/sh -c \"$remote\"\n")
			owner := &workspaceOwner{
				key: strings.Repeat("a", 64), token: strings.Repeat("b", 64),
				transport: rsyncWitnessTestTransport{workspaceOwnerTransportFunc(func(_ context.Context, req workspaceOwnerRemoteRequest) (string, error) {
					if req.Action != workspaceOwnerInspect {
						t.Errorf("unexpected action: %s", req.Action)
					}
					return response, nil
				})},
			}
			root := filepath.Join(home, ".crabbox", "workspace-owners")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			stop := filepath.Join(root, owner.key+".rsync-stop."+owner.token)
			target := SSHTarget{Host: "localhost", Port: "22", NoControlMaster: true}
			err := finishRsyncWorkspaceWitness(t.Context(), target, owner)
			if response == "OWNED" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(stop); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("quiescent stop request remains: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("unconfirmed quiescence succeeded")
			}
			if _, err := os.Stat(stop); err != nil {
				t.Fatalf("lost stop request while witness is unconfirmed: %v", err)
			}
			// A later successful inspection may retire the retained stop request.
			owner.transport = rsyncWitnessTestTransport{workspaceOwnerTransportFunc(func(context.Context, workspaceOwnerRemoteRequest) (string, error) { return "OWNED", nil })}
			if err := finishRsyncWorkspaceWitness(t.Context(), target, owner); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(stop); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stop request remains after recovery: %v", err)
			}
		})
	}
}

// Use the real rsync protocol, with only the SSH network hop replaced by a
// local shell. The registered PID must be the receiver, and a failed receiver
// must not leave a detached guard blocking registration on the reused lease.
func TestRsyncReceiverWitnessReusedLease(t *testing.T) {
	binary, err := exec.LookPath("rsync")
	if err != nil {
		t.Skip("rsync unavailable")
	}
	t.Run("default", func(t *testing.T) { testRsyncReceiverWitnessReusedLease(t, binary, binary) })
	if runtime.GOOS == "darwin" && binary != "/usr/bin/rsync" {
		t.Run("openrsync", func(t *testing.T) { testRsyncReceiverWitnessReusedLease(t, "/usr/bin/rsync", binary) })
	}
}

func testRsyncReceiverWitnessReusedLease(t *testing.T, binary, serverBinary string) {
	home, bin := t.TempDir(), t.TempDir()
	installLocalRsyncSSH(t, bin)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeExecutable(t, filepath.Join(bin, "rsync"), "#!/bin/sh\n"+`[ "$(sed -n '1p' "$child")" = "$$" ] || exit 73`+"\nexec "+shellQuote(serverBinary)+` "$@"`+"\n")
	t.Setenv("HOME", home)
	shell := filepath.Join(bin, "remote-shell")
	writeExecutable(t, shell, "#!/bin/sh\nshift\nexec /bin/sh -c \"$*\"\n")
	source, remote := t.TempDir(), filepath.Join(t.TempDir(), "remote")
	name := "payload name's\n.txt"
	key := workspaceOwnerKey("reused-rsync-receiver")
	for iteration := 0; iteration < 3; iteration++ {
		token := strings.Repeat(string(rune('a'+iteration)), 64)
		req := workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: key, Token: token, TTL: time.Minute}
		if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(req)); err != nil || out != "ACQUIRED" {
			t.Fatalf("acquire %d: %q %v", iteration, out, err)
		}
		owner := &workspaceOwner{key: key, token: token, transport: workspaceOwnerTransportFunc(func(ctx context.Context, request workspaceOwnerRemoteRequest) (string, error) {
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", remoteWorkspaceOwnerPOSIX(request))
			out, err := cmd.CombinedOutput()
			return strings.TrimSpace(string(out)), err
		})}
		mustWriteTestFile(t, filepath.Join(source, name), fmt.Sprintf("iteration %d\n", iteration))
		receiver, err := stageRsyncWorkspaceReceiver(t.Context(), SSHTarget{Host: "local", Port: "22", NoControlMaster: true}, owner)
		if err != nil {
			t.Fatal(err)
		}
		destination := remote
		if iteration == 1 {
			destination = "/dev/null/cannot-create"
		}
		cmd := exec.CommandContext(t.Context(), binary, "-a", "--checksum", "--from0", "--files-from=-", "-e", shell, "--rsync-path", receiver.command, "--", source+"/", "local:"+destination+"/")
		cmd.Stdin = strings.NewReader(name + "\x00")
		out, err := cmd.CombinedOutput()
		if settleErr := waitWorkspaceOwnerNoChild(t.Context(), owner, 5*time.Second); settleErr != nil {
			t.Fatal(settleErr)
		}
		if cleanupErr := receiver.close(t.Context(), SSHTarget{Host: "local", Port: "22", NoControlMaster: true}); cleanupErr != nil {
			t.Fatal(cleanupErr)
		}
		if _, statErr := os.Stat(filepath.Join(home, strings.TrimPrefix(receiver.command, "~/"))); !os.IsNotExist(statErr) {
			t.Fatalf("staged receiver remains: %v", statErr)
		}
		if (err != nil) != (iteration == 1) {
			t.Fatalf("transfer %d: %v\n%s", iteration, err, out)
		}
		child := filepath.Join(home, ".crabbox/workspace-owners", key+".child")
		if _, err := os.Stat(child); !os.IsNotExist(err) {
			t.Fatalf("receiver left CHILD on iteration %d: %v", iteration, err)
		}
		req.Action = workspaceOwnerRelease
		if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(req)); err != nil || out != "RELEASED" {
			t.Fatalf("release %d: %q %v", iteration, out, err)
		}
		if iteration != 1 {
			want, _ := os.ReadFile(filepath.Join(source, name))
			got, err := os.ReadFile(filepath.Join(remote, name))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("payload %d: %q %v", iteration, got, err)
			}
		}
	}
}

func TestRsyncUsesReceiverWitnessWithoutDetachedGuard(t *testing.T) {
	bin, home := t.TempDir(), t.TempDir()
	capture := filepath.Join(home, "arguments")
	t.Setenv("HOME", home)
	installLocalRsyncSSH(t, bin)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_TEST_RSYNC_ARGUMENTS", capture)
	writeExecutable(t, filepath.Join(bin, "rsync"), `#!/bin/sh
if [ "$1" = --version ]; then printf 'rsync version 3.4.1 protocol version 32\n'; exit 0; fi
printf '%s\n' "$@" > "$CRABBOX_TEST_RSYNC_ARGUMENTS"
cat >/dev/null
`)
	owner := &workspaceOwner{key: strings.Repeat("a", 64), token: strings.Repeat("b", 64), transport: rsyncWitnessTestTransport{workspaceOwnerTransportFunc(func(_ context.Context, req workspaceOwnerRemoteRequest) (string, error) {
		if req.Action != workspaceOwnerInspect {
			t.Errorf("unexpected action %s", req.Action)
		}
		return "OWNED", nil
	})}}
	target := SSHTarget{Host: "127.0.0.1", Port: "1", User: "fixture", TargetOS: targetLinux, NoControlMaster: true}
	req := workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: owner.key, Token: owner.token, TTL: time.Minute}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(req)); err != nil || out != "ACQUIRED" {
		t.Fatalf("acquire: %s %v", out, err)
	}
	ctx := contextWithWorkspaceOwner(t.Context(), owner)
	if err := rsync(ctx, target, t.TempDir(), "/work/fixture", nil, io.Discard, io.Discard, rsyncOptions{UseFilesFrom: true, FilesFrom: []byte("file\x00")}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(args), "\n")
	found := false
	for i, line := range lines {
		if line == "--rsync-path" && i+1 < len(lines) {
			path := lines[i+1]
			found = strings.HasPrefix(path, "~/.crabbox/workspace-owners/"+owner.key+".rsync."+owner.token+".") && !strings.ContainsAny(path, " \t\r\n'\"")
			if _, err := os.Stat(filepath.Join(home, strings.TrimPrefix(path, "~/"))); !os.IsNotExist(err) {
				t.Fatalf("unused staged receiver remains: %v", err)
			}
		}
	}
	if !found {
		t.Fatalf("receiver is not a private single-word path: %s", args)
	}
}

func installLocalRsyncSSH(t *testing.T, bin string) {
	t.Helper()
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("SSH unavailable")
	}
	writeExecutable(t, filepath.Join(bin, "ssh"), "#!/bin/sh\nfor arg do\n if [ \"$arg\" = -G ]; then exec "+shellQuote(ssh)+" \"$@\"; fi\ndone\nfor arg do remote=$arg; done\nexec /bin/sh -c \"$remote\"\n")
}
