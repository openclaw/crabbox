//go:build !windows

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestCheckpointCaptureProcessOwnership(t *testing.T) {
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
			// Independent safety cleanup makes a red regression safe. It is not
			// credited as harness cleanup, and only touches the observed group.
			finished := false
			rescue := func() {
				if finished {
					return
				}
				if current, err := syscall.Getpgid(child); err == nil && current == group {
					_ = syscall.Kill(-group, syscall.SIGKILL)
				}
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
			finished = true
			t.Logf("parent=%d child=%d group=%d elapsed=%s wait=%v childAbsent=true", p.cmd.Process.Pid, child, group, time.Since(started), p.err)
		})
	}
}

func TestCheckpointCaptureProcessCleanupIsolation(t *testing.T) {
	for _, mode := range []string{"concurrent cancellation", "late event after exit"} {
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
