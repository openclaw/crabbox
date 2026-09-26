//go:build darwin || linux

package ssh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestStaticPowerCancellationJoinsGrandchild(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			cfg, dir := staticPowerFixture(t)
			cfg.Static.ID = "static_cancel"
			pidfile := filepath.Join(dir, "child")
			// The shell starts a child shell, which starts the sleep grandchild. All
			// three must leave the joined group before the host lock is released.
			script := fmt.Sprintf("#!/bin/sh\n/bin/sh -c 'sleep 60 & echo $! > \"$1\"; wait' child %q &\nwait\n", pidfile)
			if err := os.WriteFile(cfg.Static.StartCommand[0], []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			cfg.Static.StopCommand = []string{"/usr/bin/true"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			expected := context.Canceled
			if deadline {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 1200*time.Millisecond)
				defer deadlineCancel()
				expected = context.DeadlineExceeded
			}
			done := make(chan error, 1)
			b := powerBackend(cfg)
			go func() { _, err := b.Acquire(ctx, core.AcquireRequest{Repo: core.Repo{Root: dir}}); done <- err }()
			var pid int
			until := time.Now().Add(5 * time.Second)
			for time.Now().Before(until) {
				data, _ := os.ReadFile(pidfile)
				pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
				if pid > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if pid == 0 {
				t.Fatal("grandchild did not start")
			}
			if !deadline {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, expected) || core.ExitCodeForError(err, 0) != 1 {
					t.Fatalf("err=%v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("hook cancellation did not join")
			}
			// A zombie can briefly await its init reaper on Linux; it cannot execute.
			if err := syscall.Kill(pid, 0); err == nil {
				stat, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
				if !strings.Contains(string(stat), ") Z ") {
					t.Fatalf("grandchild %d survived hook return", pid)
				}
			}
			h, unlock, err := b.lockPowerHost(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			state := h.record.References[cfg.Static.ID].State
			unlock()
			if state != "pending-stop" {
				t.Fatalf("state=%s", state)
			}
			lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: cfg.Static.ID})
			if err != nil {
				t.Fatal(err)
			}
			if err = releasePower(b, lease); err != nil {
				t.Fatal(err)
			}
		})
	}
}
