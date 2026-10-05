//go:build !windows

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
