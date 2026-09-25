//go:build !windows

package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLeaseTelemetryPreservesWorkspaceWitness(t *testing.T) {
	for _, phase := range []string{"live-workload", "exited-marker-reader"} {
		t.Run(phase, func(t *testing.T) {
			home, owner := workspaceOwnerSetupFixture(t)
			tools := t.TempDir()
			writeExecutable(t, filepath.Join(tools, "ssh"), "#!/bin/sh\nfor arg; do remote=\"$arg\"; done\nHOME="+shellQuote(home)+"\nexport HOME\nexec /bin/sh -c \"$remote\"\n")
			t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			target := SSHTarget{Host: "127.0.0.1", User: "runner", Port: "22", FallbackPorts: []string{}, TargetOS: targetLinux, NoControlMaster: true}

			ready, release := filepath.Join(home, "ready"), filepath.Join(home, "release")
			if err := syscall.Mkfifo(release, 0o600); err != nil {
				t.Fatal(err)
			}
			barrier, err := os.OpenFile(release, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer barrier.Close()
			wait := "touch " + shellQuote(ready) + "\nread -r resumed < " + shellQuote(release) + "\n"
			payload := wait
			if phase == "exited-marker-reader" {
				payload = `cat "$HOME/missing-marker" 2>/dev/null || true`
			}
			script := remoteWorkspaceOwnerPOSIXWitnessScript(owner.key, owner.token, payload, "")
			if phase == "exited-marker-reader" {
				// Pause only after the child exits and before acquiring the cleanup
				// gate: telemetry must not replace the still-recorded child identity.
				offset := strings.LastIndex(script, "\nrun_owner_gate ")
				if offset < 0 {
					t.Fatal("witness cleanup gate missing")
				}
				script = script[:offset] + "\n" + wait + script[offset:]
			}
			cmd, ctx := boundedWorkspaceOwnerCommand(t, home, os.Getenv("PATH"), script)
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waitForWorkspaceOwnerTestFile(t, ready)
			childPath := filepath.Join(home, ".crabbox", "workspace-owners", owner.key+".child")
			before, beforeErr := os.ReadFile(childPath)
			ownedCtx := contextWithWorkspaceOwner(ctx, owner)
			telemetry, telemetryErr := collectLeaseTelemetry(ownedCtx, target)
			after, afterErr := os.ReadFile(childPath)
			var competingErr error
			if phase == "live-workload" {
				_, competingErr = runSSHOutput(ownedCtx, target, "printf competing-command")
			}
			_, releaseErr := barrier.WriteString("continue\n")
			commandErr := cmd.Wait()

			if commandErr != nil || ctx.Err() != nil || output.Len() != 0 {
				t.Errorf("original command: exit=%d err=%v context=%v output=%q", exitCode(commandErr), commandErr, ctx.Err(), output.String())
			}
			if releaseErr != nil {
				t.Errorf("release barrier: %v", releaseErr)
			}
			if telemetryErr != nil || telemetry == nil {
				t.Errorf("telemetry observation failed: %v", telemetryErr)
			}
			if beforeErr != nil || afterErr != nil || len(before) == 0 || !bytes.Equal(before, after) {
				t.Errorf("telemetry changed child witness: before=%v after=%v", beforeErr, afterErr)
			}
			if phase == "live-workload" && exitCode(competingErr) != 75 {
				t.Errorf("competing owned command: exit=%d err=%v", exitCode(competingErr), competingErr)
			}
			for _, step := range []struct {
				action workspaceOwnerAction
				want   string
			}{{workspaceOwnerInspect, "OWNED"}, {workspaceOwnerRelease, "RELEASED"}} {
				req := workspaceOwnerRemoteRequest{Action: step.action, Key: owner.key, Token: owner.token, TTL: time.Minute}
				if got, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(req)); err != nil || got != step.want {
					t.Errorf("owner %s: got=%q err=%v", step.action, got, err)
				}
			}
		})
	}
}

func TestLeaseTelemetryPreservesCallerCancellation(t *testing.T) {
	for _, cause := range []string{"cancel", "deadline"} {
		t.Run(cause, func(t *testing.T) {
			tools := t.TempDir()
			ready := filepath.Join(tools, "ready")
			writeExecutable(t, filepath.Join(tools, "ssh"), "#!/bin/sh\ntouch "+shellQuote(ready)+"\nexec sleep 30\n")
			t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			ctx = contextWithWorkspaceOwner(ctx, &workspaceOwner{key: workspaceOwnerKey("telemetry"), token: strings.Repeat("a", 64)})
			target := SSHTarget{Host: "127.0.0.1", User: "runner", Port: "22", FallbackPorts: []string{}, TargetOS: targetLinux, NoControlMaster: true}
			done := make(chan *LeaseTelemetry, 1)
			go func() {
				done <- collectLeaseTelemetryBestEffort(ctx, leaseTelemetryCollectorForTarget(target))
			}()
			waitForWorkspaceOwnerTestFile(t, ready)
			want := context.DeadlineExceeded
			if cause == "cancel" {
				want = context.Canceled
				cancel()
			}
			select {
			case telemetry := <-done:
				if telemetry != nil || ctx.Err() != want {
					t.Fatalf("telemetry=%v context=%v want=%v", telemetry, ctx.Err(), want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("telemetry detached from the caller's earlier cancellation")
			}
		})
	}
}

func TestCoordinatorHeartbeatStopJoinsOwnedTelemetry(t *testing.T) {
	tools := t.TempDir()
	ready := filepath.Join(tools, "ready")
	writeExecutable(t, filepath.Join(tools, "ssh"), "#!/bin/sh\ntouch "+shellQuote(ready)+"\nexec sleep 30\n")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	ctx := contextWithWorkspaceOwner(context.Background(), &workspaceOwner{key: workspaceOwnerKey("telemetry"), token: strings.Repeat("a", 64)})
	target := SSHTarget{Host: "127.0.0.1", User: "runner", Port: "22", FallbackPorts: []string{}, TargetOS: targetLinux, NoControlMaster: true}
	stop, err := startCoordinatorHeartbeat(ctx, client, "cbx_telemetry", "aws", 30*time.Minute, nil, leaseTelemetryCollectorForTarget(target), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	waitForWorkspaceOwnerTestFile(t, ready)
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("heartbeat stop did not cancel and join its telemetry sample")
	}
}
