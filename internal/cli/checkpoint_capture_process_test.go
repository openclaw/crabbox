//go:build !windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func runCheckpointNativeLifetimeContract(t *testing.T, repo, binary string) {
	t.Parallel()
	t.Run("paused native helper exits when invocation lifetime closes", func(t *testing.T) {
		f := newCheckpointCaptureFixture(t, repo, binary)
		state := f.state()
		state.Pause = "inventory"
		f.writeState(state)
		path := filepath.Join(f.root, "helper-lifetime.fifo")
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		owner, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary+".provider", "--", "machine0", "ls", "--json")
		cmd.Env = append(append([]string(nil), f.env...), "CRABBOX_CAPTURE_LIFETIME="+path)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case event := <-f.events:
			if event.Kind != "inventory" || event.PID != cmd.Process.Pid {
				t.Fatal("wrong helper reached lifetime probe")
			}
		case err := <-done:
			t.Fatalf("helper exited before pause: %v %s", err, output.String())
		case <-ctx.Done():
			t.Fatalf("helper never paused: %v", <-done)
		}
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err == nil || ctx.Err() != nil {
			t.Fatalf("lifetime closure failed to end helper: err=%v context=%v", err, ctx.Err())
		}
	})
	for _, missing := range []bool{true, false} {
		t.Run("native helper rejects ended lifetime missing="+strconv.FormatBool(missing), func(t *testing.T) {
			f := newCheckpointCaptureFixture(t, repo, binary)
			before, err := os.ReadFile(filepath.Join(f.root, "provider.json"))
			if err != nil {
				t.Fatal(err)
			}
			lifetime := filepath.Join(f.root, "ended-lifetime.fifo")
			if !missing {
				if err := syscall.Mkfifo(lifetime, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary+".provider", "--", "machine0", "ls", "--json")
			cmd.Env = append(append([]string(nil), f.env...), "CRABBOX_CAPTURE_LIFETIME="+lifetime)
			output, err := cmd.CombinedOutput()
			if err == nil || ctx.Err() != nil || !bytes.Contains(output, []byte("lifetime")) {
				t.Fatalf("late helper err=%v context=%v output=%s", err, ctx.Err(), output)
			}
			after, err := os.ReadFile(filepath.Join(f.root, "provider.json"))
			if err != nil || !bytes.Equal(before, after) || len(f.eventLog()) != 0 {
				t.Fatalf("late helper reached native effects: err=%v", err)
			}
		})
	}
}

func TestCheckpointCaptureProcessOwnership(t *testing.T) {
	// Each case owns its environment, FIFOs, and process groups; the real
	// 15-second deadlines can run alongside other independent network waits.
	t.Parallel()
	for _, tc := range []struct {
		name       string
		deadline   bool
		pipes      bool
		terminated bool
	}{
		{"deadline", true, false, false},
		{"deadline with inherited pipes", true, true, false},
		{"exited parent", false, false, false},
		{"exited parent with inherited pipes", false, true, false},
		{"terminated anchor", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			gate := filepath.Join(root, "exit.fifo")
			if err := syscall.Mkfifo(gate, 0o600); err != nil {
				t.Fatal(err)
			}
			// O_RDWR lets the test release the parent without an unbounded open.
			fd, err := syscall.Open(gate, syscall.O_RDWR|syscall.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			release := os.NewFile(uintptr(fd), gate)
			defer release.Close()
			redirect := " >/dev/null 2>&1"
			if tc.pipes {
				redirect = ""
			}
			end := `read line < "$2"; exit 0`
			prefix := ""
			if tc.deadline || tc.terminated {
				end = "wait"
			}
			if tc.terminated {
				prefix = `trap '' TERM; `
				end = `read line < "$2"; wait "$child"; exit 7`
			}
			f := &checkpointCaptureFixture{t: t, root: root, repo: root, binary: "/bin/sh", env: []string{"PATH=/usr/bin:/bin"}}
			started := time.Now()
			p := f.start("child-ready", "-c", prefix+`sleep 60`+redirect+` & child=$!; printf '%s\n' "$child" > "$1"; printf child-ready >&2; `+end, "ownership-fixture", filepath.Join(root, "child.pid"), gate)
			p.waitMarker(t)
			child := readPIDFile(t, filepath.Join(root, "child.pid"))
			group, err := syscall.Getpgid(child)
			parentGroup, parentErr := syscall.Getpgid(p.cmd.Process.Pid)
			if err != nil || parentErr != nil || group != parentGroup || group == syscall.Getpgrp() {
				t.Fatalf("child=%d group=%d parent=%d group=%d errors=%v/%v", child, group, p.cmd.Process.Pid, parentGroup, err, parentErr)
			}
			// Rescue uses the retained anchor too; an observed numeric group
			// is not signal authority once Wait has sealed that ownership.
			rescue := func() {
				p.closeNativeHelpers()
				_ = p.signalGroup(syscall.SIGKILL)
			}
			t.Cleanup(rescue)
			if tc.terminated {
				// Establish child exit explicitly while the parent is held at
				// the FIFO, without depending on inherited SIGTERM dispositions.
				if err := syscall.Kill(child, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				if err := p.signalGroup(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				// Keep the invocation alive until its anchor is definitely a
				// zombie; then its exit leaves no live group member to signal.
				deadline := time.Now().Add(5 * time.Second)
				for {
					state, err := systemInspectionCommand("ps", "-p", strconv.Itoa(group), "-o", "stat=").Output()
					if err == nil && strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("anchor %d never became a zombie: %q %v", group, state, err)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			if !tc.deadline {
				if _, err := release.Write([]byte("exit\n")); err != nil {
					t.Fatal(err)
				}
			}
			// The helper's existing 15s deadline is exercised unchanged. The
			// extra bound only reports/rescues a broken Wait; it never passes it.
			select {
			case <-p.done:
			case <-time.After(20 * time.Second):
				t.Errorf("Wait stuck with descendant pipes: parent=%d child=%d group=%d elapsed=%s", p.cmd.Process.Pid, child, group, time.Since(started))
				rescue()
				<-p.done
			}
			if (tc.deadline || tc.terminated) && p.err == nil {
				t.Error("deadline unexpectedly succeeded")
			} else if !tc.deadline && !tc.terminated && p.err != nil {
				t.Errorf("natural parent exit: %v", p.err)
			}
			if tc.terminated && p.cmd.ProcessState.ExitCode() != 7 {
				t.Errorf("intentional exit status was lost: %v", p.err)
			}
			f.kill(p) // Must still own descendants after the leader was reaped.
			deadline := time.Now().Add(time.Second)
			for processAlive(child) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if processAlive(child) {
				t.Errorf("descendant survived harness cleanup: parent=%d child=%d group=%d elapsed=%s", p.cmd.Process.Pid, child, group, time.Since(started))
				rescue()
			}
			waitForPondMeshSignalProcessExit(t, child)
			t.Logf("parent=%d child=%d group=%d elapsed=%s wait=%v childAbsent=true", p.cmd.Process.Pid, child, group, time.Since(started), p.err)
		})
	}
}

func TestCheckpointCaptureProcessCleanupIsolation(t *testing.T) {
	for _, mode := range []string{"concurrent cancellation", "late event after exit", "stale helper event"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			f := &checkpointCaptureFixture{t: t, root: root, repo: root, binary: "/bin/sh", env: []string{"PATH=/usr/bin:/bin"}}
			guard := f.start("", "-c", "exec sleep 60")
			command := "exec sleep 60"
			if mode == "late event after exit" {
				command = "exit 0"
			}
			p := f.start("", "-c", command)
			guardGroup := guard.owner.platform.anchor.Process.Pid
			events := []checkpointCaptureFixtureEvent{
				{PID: guardGroup, PGID: guardGroup, Owner: guard.cmd.Process.Pid},
				{PID: guard.cmd.Process.Pid, PGID: guardGroup, Owner: p.cmd.Process.Pid},
				{PID: syscall.Getpgrp(), PGID: syscall.Getpgrp(), Owner: p.cmd.Process.Pid},
			}
			if mode == "late event after exit" {
				<-p.done
				if p.err != nil {
					t.Fatal(p.err)
				}
				// Model a later event whose owner number has been reused. A
				// completed invocation must never interpret new ownership claims.
				events = append(events, checkpointCaptureFixtureEvent{PID: guardGroup, PGID: guardGroup, Owner: p.cmd.Process.Pid})
			}
			if mode == "stale helper event" {
				// Model a completed helper's PGID reused while its owner still runs.
				// Both groups remain test-owned, so the failing regression is safe.
				events = append(events, checkpointCaptureFixtureEvent{PID: guardGroup, PGID: guardGroup, Owner: p.cmd.Process.Pid})
			}
			var log bytes.Buffer
			for _, event := range events {
				if err := json.NewEncoder(&log).Encode(event); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "events.jsonl"), log.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			var workers sync.WaitGroup
			for _, cleanup := range []func(){p.cancel, func() { f.kill(p) }, func() { f.kill(p) }} {
				workers.Go(cleanup)
			}
			workers.Wait()
			if !processAlive(guard.cmd.Process.Pid) || !processAlive(guardGroup) {
				t.Fatal("cleanup killed a different invocation")
			}
			if processAlive(p.cmd.Process.Pid) || processAlive(p.owner.platform.anchor.Process.Pid) {
				t.Fatal("cleanup did not reap the invocation and anchor")
			}
			t.Logf("cleaned pid=%d pgid=%d; guard pid=%d pgid=%d survived", p.cmd.Process.Pid, p.owner.platform.anchor.Process.Pid, guard.cmd.Process.Pid, guardGroup)
		})
	}
}
