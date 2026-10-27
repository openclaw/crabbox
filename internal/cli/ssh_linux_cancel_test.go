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
	home, owner := workspaceOwnerSetupFixture(t)
	tools := t.TempDir()
	writeExecutable(t, filepath.Join(tools, "ssh"), "#!/bin/sh\nfor arg; do remote=\"$arg\"; done\nHOME="+shellQuote(home)+"\nexport HOME\nexec setsid --fork --wait /bin/sh -c \"$remote\"\n")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	ready := filepath.Join(home, "started")
	pids := filepath.Join(home, "pids")
	payload := "echo $$ > " + shellQuote(pids) + "; (trap '' TERM; exec sleep 300) & echo $! >> " + shellQuote(pids) + "; touch " + shellQuote(ready) + "; wait"
	target := SSHTarget{Host: "fixture.invalid", User: "runner", Port: "22", TargetOS: targetLinux, NoControlMaster: true}
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
