package cli

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestSSHCredentialPreparationPreventsDispatch(t *testing.T) {
	cause := errors.New("provider SSH credentials expired; refresh denied")
	for _, transport := range []string{"command", "owner renewal", "copy", "tunnel", "exported tunnel"} {
		t.Run(transport, func(t *testing.T) {
			calls := 0
			target := SSHTarget{Host: "127.0.0.1", Port: "22", User: "test", AuthoritativeKnownHosts: true,
				PrepareConnection: func(context.Context) error { calls++; return cause }}
			var err error
			var cmd *exec.Cmd
			switch transport {
			case "command":
				cmd = sshCommandContext(context.Background(), target, "unused")
				err = cmd.Run()
			case "owner renewal":
				_, err = (sshWorkspaceOwnerTransport{target: target}).Do(context.Background(), workspaceOwnerRemoteRequest{Action: workspaceOwnerRenew, Key: "key", Token: "token"})
			case "copy":
				handle, createErr := resolvedRsyncCommandForGOOS(context.Background(), "linux", target, []string{"unused"}, "", "")
				if createErr != nil {
					t.Fatal(createErr)
				}
				cmd = handle.cmd
				err = handle.Start()
			case "tunnel":
				handle := newOwnedSSHTransportCommand(context.Background(), target, []string{"unused"})
				cmd = handle.cmd
				err = handle.Start()
			case "exported tunnel":
				cmd = pondMeshDaemonCommand(target, "ssh", "unused")
				err = cmd.Run()
			}
			if err == nil || !strings.Contains(err.Error(), cause.Error()) || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if cmd != nil && cmd.Process != nil {
				t.Fatal("dispatched despite credential error")
			}
		})
	}
}

func TestWorkspaceOwnerCredentialPreparationNamesCause(t *testing.T) {
	cause := errors.New("Tenki SSH credentials expired; the command outlived them")
	for _, wrapped := range []bool{false, true} {
		var err error = cause
		if wrapped {
			err = sshPreparationError{cause}
		}
		got := workspaceOwnerCallError("release remote workspace owner", err).Error()
		if !strings.Contains(got, cause.Error()) || strings.Contains(got, "ambiguous remote state") == wrapped {
			t.Fatalf("prepared=%t error=%s", wrapped, got)
		}
	}
}
