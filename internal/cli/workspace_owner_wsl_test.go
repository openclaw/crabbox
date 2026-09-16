package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type budgetedWSL2OwnerTestTransport struct {
	workspaceOwnerTransportFunc
	budget time.Duration
}

func (t budgetedWSL2OwnerTestTransport) CallBudget() time.Duration { return t.budget }

func TestWorkspaceOwnerWSL2RenewalBudgetAndPayload(t *testing.T) {
	target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	req := workspaceOwnerRemoteRequest{Action: workspaceOwnerRenew, Key: workspaceOwnerKey("cbx_busy"), Token: strings.Repeat("a", 64), TTL: 10 * time.Minute}
	captureWSLStage(t, strings.Repeat("a", 32), func(spool *wslStageSpool, _ *SSHTarget, timing wslStageTiming, data []byte) {
		_, _, command, _ := decodeWSLStage(t, data)
		if len(command) > 1800 {
			t.Fatalf("renewal stages %d bytes; want compact marker-only helper", len(command))
		}
		if timing.operation < 86*time.Second {
			t.Fatalf("renewal guard=%s; want load allowance", timing.operation)
		}
		if strings.Contains(command, "base64") || strings.Contains(command, "ps -") {
			t.Fatal("renewal repeats script staging or inspects processes")
		}
	})
	installWorkspaceOwnerRecordingSSH(t)
	t.Setenv("CRABBOX_OWNER_SSH_SUCCESS_STDOUT", "RENEWED")
	if got, err := (sshWorkspaceOwnerTransport{target: target}).Do(t.Context(), req); err != nil || got != "RENEWED" {
		t.Fatalf("response=%q err=%v", got, err)
	}
}

func TestWorkspaceOwnerWSL2RenewalRetriesOnlyConfirmedContention(t *testing.T) {
	for _, tc := range []struct {
		name, response, code string
		calls                int
		recovered            bool
	}{
		{"slow lock recovers", "BUSY", "0", 2, true},
		{"lock remains busy", "BUSY", "0", 3, false},
		{"transport lost", "", "255", 1, false},
		{"busy with failed transport", "BUSY", "1", 1, false},
		{"token lost", "MISMATCH", "75", 1, false},
		{"expired", "EXPIRED", "75", 1, false},
		{"ambiguous", "AMBIGUOUS", "74", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := installWorkspaceOwnerRecordingSSH(t)
			t.Setenv("CRABBOX_OWNER_SSH_FAIL_CALL", "1")
			t.Setenv("CRABBOX_OWNER_SSH_FAIL_CODE", tc.code)
			t.Setenv("CRABBOX_OWNER_SSH_FAIL_STDOUT", tc.response)
			next := tc.response
			if tc.recovered {
				next = "RENEWED"
			}
			t.Setenv("CRABBOX_OWNER_SSH_SUCCESS_STDOUT", next)
			stages := 0
			var deadline time.Time
			captureWSLStage(t, strings.Repeat("a", 32), func(_ *wslStageSpool, _ *SSHTarget, _ wslStageTiming, _ []byte) { stages++ })
			target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2, Port: "22", FallbackPorts: []string{}}
			transport := sshWorkspaceOwnerTransport{target: target}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			owner := &workspaceOwner{target: target, key: workspaceOwnerKey("cbx_busy"), token: strings.Repeat("a", 64), ttl: 10 * time.Minute, ctx: ctx, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{})}
			owner.transport = budgetedWSL2OwnerTestTransport{workspaceOwnerTransportFunc(func(ctx context.Context, req workspaceOwnerRemoteRequest) (string, error) {
				deadline, _ = ctx.Deadline()
				got, err := transport.Do(ctx, req)
				if tc.recovered {
					close(owner.stop)
				}
				return got, err
			}), transport.CallBudget()}
			ticks := make(chan time.Time, 1)
			ticks <- time.Now()
			owner.renewLoopWithTicks(ticks, transport.CallBudget())
			count, err := os.ReadFile(filepath.Join(dir, "count"))
			sshCalls := tc.calls
			if tc.code != "0" {
				sshCalls++
			} // Exact stage cleanup after failed execution.
			if err != nil || string(count) != fmt.Sprint(sshCalls) || stages != tc.calls {
				t.Fatalf("SSH=%s stages=%d err=%v", count, stages, err)
			}
			if deadline.IsZero() || time.Until(deadline) > owner.ttl {
				t.Fatal("retry escaped owner expiry window")
			}
			if tc.recovered {
				if owner.Err() != nil || ctx.Err() != nil {
					t.Fatalf("successful retry canceled owner: %v", owner.Err())
				}
			} else {
				if owner.Err() == nil || ctx.Err() != context.Canceled {
					t.Fatalf("failed renewal did not cancel workload: %v", owner.Err())
				}
				if owner.ConfirmNoChild(t.Context()) != owner.Err() {
					t.Fatal("collection ignored lost ownership")
				}
			}
		})
	}
}

func TestWorkspaceOwnerWSL2RenewalMarkerProtocol(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	token := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, marker, lockCode, want string
		changes                      bool
	}{
		{"owned", "v1\n" + token + "\n4102444800\n", "0", "RENEWED", true},
		{"mismatch", "v1\n" + strings.Repeat("b", 64) + "\n4102444800\n", "0", "MISMATCH", false},
		{"expired", "v1\n" + token + "\n1\n", "0", "EXPIRED", false},
		{"malformed", "v1\n" + token + "\ninvalid\n", "0", "AMBIGUOUS", false},
		{"missing expiry", "v1\n" + token + "\n", "0", "AMBIGUOUS", false},
		{"busy", "v1\n" + token + "\n4102444800\n", "75", "BUSY", false},
		{"lock failure", "v1\n" + token + "\n4102444800\n", "73", "AMBIGUOUS", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, ".crabbox", "workspace-owners")
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			key := workspaceOwnerKey("cbx_marker")
			state := filepath.Join(root, key+".owner")
			if err := os.WriteFile(state, []byte(tc.marker), 0600); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			writeExecutable(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit "+tc.lockCode+"\n")
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			command := remoteWorkspaceOwnerCommand(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, workspaceOwnerRemoteRequest{Action: workspaceOwnerRenew, Key: key, Token: token, TTL: time.Minute})
			got, err := runPOSIXWorkspaceOwnerScript(t, home, command)
			if got != tc.want || (err == nil) != (tc.want == "RENEWED" || tc.want == "BUSY") {
				t.Fatalf("got=%q err=%v want=%s", got, err, tc.want)
			}
			after, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			if (string(after) != tc.marker) != tc.changes {
				t.Fatal("unexpected marker mutation")
			}
			info, err := os.Stat(state)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("marker permissions changed")
			}
		})
	}
}
