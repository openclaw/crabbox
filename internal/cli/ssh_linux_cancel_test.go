//go:build linux

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The transport executes the real witness/supervisor and real descendants. It
// substitutes only the SSH network hop; live-provider proof covers that hop.
func TestLinuxSSHCommandCancellationRetiresDescendantsAndWorkspace(t *testing.T) {
	home, owner, target := linuxCommandSSHFixture(t)
	ready := filepath.Join(home, "started")
	pids := filepath.Join(home, "pids")
	grace := filepath.Join(home, "term-cleanup")
	payload := "trap " + shellQuote("sleep 1; touch "+shellQuote(grace)+"; exit 130") + " TERM; echo $$ > " + shellQuote(pids) + "; (trap '' TERM; exec sleep 300) & echo $! >> " + shellQuote(pids) + "; touch " + shellQuote(ready) + "; wait"
	ctx, cancel := context.WithTimeout(contextWithWorkspaceOwner(t.Context(), owner), 30*time.Second)
	defer cancel()
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := runSSHStreamResult(ctx, target, "exec bash -c "+shellQuote(payload), &output, &output)
		done <- err
	}()
	waitForTestFile(t, ready, 10*time.Second)
	data, err := os.ReadFile(pids)
	if err != nil {
		t.Fatal(err)
	}
	children := map[int]string{}
	for _, field := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			t.Fatal(err)
		}
		children[pid] = strings.Fields(string(raw[strings.LastIndex(string(raw), ")")+1:]))[19]
	}
	t.Cleanup(func() {
		for pid, start := range children {
			raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if err == nil && strings.Fields(string(raw[strings.LastIndex(string(raw), ")")+1:]))[19] == start {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation reported success")
		}
	case <-time.After(25 * time.Second):
		t.Fatal("cancellation did not finish within its cleanup bound")
	}
	if _, err := os.Stat(grace); err != nil {
		t.Errorf("TERM cleanup did not get its grace: %v", err)
	}
	for pid := range children {
		if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			state := strings.Fields(string(raw[strings.LastIndex(string(raw), ")")+1:]))[0]
			if state != "Z" {
				t.Errorf("owned command process %d survived cancellation: %s", pid, state)
			}
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	check, stop := context.WithTimeout(contextWithWorkspaceOwner(t.Context(), owner), 15*time.Second)
	defer stop()
	var next bytes.Buffer
	code, err := runSSHStreamResult(check, target, "printf next-command", &next, &next)
	if err != nil || code != 0 || next.String() != "next-command" {
		t.Fatalf("workspace was not reusable: code=%d err=%v output=%q", code, err, next.String())
	}
}

func linuxCommandSSHFixture(t *testing.T) (string, *workspaceOwner, SSHTarget) {
	t.Helper()
	home, owner := workspaceOwnerSetupFixture(t)
	tools := t.TempDir()
	writeExecutable(t, filepath.Join(tools, "ssh"), "#!/bin/sh\nfor arg; do remote=\"$arg\"; done\nHOME="+shellQuote(home)+"\nexport HOME\nexec setsid --fork --wait /bin/sh -c \"$remote\"\n")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	return home, owner, SSHTarget{Host: "fixture.invalid", User: "runner", Port: "22", TargetOS: targetLinux, NoControlMaster: true}
}

// Completion must not pay the cancellation grace, but still owns and retires
// background descendants and preserves the exact command status and output.
func TestLinuxSSHCommandCompletionDoesNotWaitForCancellationGrace(t *testing.T) {
	for _, tc := range []struct {
		name       string
		code       int
		background bool
	}{
		{name: "success"}, {name: "nonzero", code: 37}, {name: "background", background: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, owner, target := linuxCommandSSHFixture(t)
			pidfile := filepath.Join(home, "background-pid")
			command := "printf done; exit " + strconv.Itoa(tc.code)
			if tc.background {
				command = "(trap '' TERM; exec sleep 300) >/dev/null 2>&1 & echo $! > " + shellQuote(pidfile) + "; " + command
			}
			ctx, cancel := context.WithTimeout(contextWithWorkspaceOwner(t.Context(), owner), 12*time.Second)
			defer cancel()
			var output bytes.Buffer
			started := time.Now()
			code, err := runSSHStreamResult(ctx, target, "exec bash -c "+shellQuote(command), &output, &output)
			elapsed := time.Since(started)
			t.Logf("command completed in %s", elapsed)
			if tc.background {
				raw, err := os.ReadFile(pidfile)
				if err != nil {
					t.Fatal(err)
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
				if err != nil {
					t.Fatal(err)
				}
				stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
				if err == nil {
					fields := strings.Fields(string(stat[strings.LastIndex(string(stat), ")")+1:]))
					if fields[0] != "Z" && fields[0] != "X" {
						start := fields[19]
						t.Cleanup(func() {
							v, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
							if e == nil && strings.Fields(string(v[strings.LastIndex(string(v), ")")+1:]))[19] == start {
								_ = syscall.Kill(pid, syscall.SIGKILL)
							}
						})
						t.Errorf("background descendant %d survived command completion", pid)
					}
				} else if !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			if code != tc.code || (err == nil) != (tc.code == 0) || output.String() != "done" {
				t.Fatalf("code=%d err=%v output=%q", code, err, output.String())
			}
			if elapsed >= 3*time.Second {
				t.Errorf("completed command waited %s; expected under 3s", elapsed)
			}
		})
	}
}
