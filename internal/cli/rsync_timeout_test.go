//go:build !windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRsyncStallTimeoutArguments(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	writeExecutable(t, filepath.Join(dir, "rsync"), "#!/bin/sh\nprintf '%s\\n' \"$@\" > "+shellQuote(log)+"\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		timeout time.Duration
		want    string
	}{{0, ""}, {-1, ""}, {time.Nanosecond, "--timeout=1"}, {1500 * time.Millisecond, "--timeout=2"}, {15 * time.Minute, "--timeout=900"}} {
		t.Run(tc.timeout.String(), func(t *testing.T) {
			if err := rsync(t.Context(), SSHTarget{Host: "localhost", User: "runner", Port: "22"}, dir, "/work", nil, io.Discard, io.Discard, rsyncOptions{Timeout: tc.timeout}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			var got string
			for _, arg := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(arg, "--timeout=") {
					got = arg
				}
			}
			if got != tc.want {
				t.Fatalf("timeout argument=%q want=%q", got, tc.want)
			}
		})
	}
}

// Exercise the real rsync protocol through a local shell stand-in for SSH.
// The quiet transfer must outlive its idle budget; log/heartbeat output is not
// progress. A peer that never sends protocol bytes must still time out.
func TestRsyncStallTimeoutBehavior(t *testing.T) {
	realRsync, err := exec.LookPath("rsync")
	if err != nil {
		t.Skip("rsync not installed")
	}
	for _, stalled := range []bool{false, true} {
		name := "progressing"
		if stalled {
			name = "stalled"
		}
		t.Run(name, func(t *testing.T) {
			bin, src, dst := t.TempDir(), t.TempDir(), t.TempDir()
			data := bytes.Repeat([]byte("synthetic source content\n"), 24000)
			if err := os.WriteFile(filepath.Join(src, "source"), data, 0o644); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, filepath.Join(bin, "rsync"), "#!/bin/sh\nexec "+shellQuote(realRsync)+" --bwlimit=128 \"$@\"\n")
			ssh := "#!/bin/sh\nwhile [ \"$#\" -gt 0 ] && [ \"$1\" != rsync ]; do shift; done\n[ \"$#\" -gt 0 ] || exit 90\nshift\nexec " + shellQuote(realRsync) + " \"$@\"\n"
			if stalled {
				ssh = "#!/bin/sh\nexec sleep 60\n"
			}
			writeExecutable(t, filepath.Join(bin, "ssh"), ssh)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			var output bytes.Buffer
			start := time.Now()
			err := rsync(ctx, SSHTarget{Host: "localhost", User: "runner", Port: "22"}, src, dst, nil, &output, &output, rsyncOptions{Compression: "never", Timeout: 2 * time.Second})
			if ctx.Err() != nil {
				t.Fatalf("parent deadline fired: %v\n%s", err, output.String())
			}
			if stalled {
				if err == nil || !strings.Contains(err.Error(), "no I/O progress") {
					t.Fatalf("missing stall diagnostic: %v\n%s", err, output.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("progressing transfer failed: %v\n%s", err, output.String())
			}
			if time.Since(start) <= 2*time.Second {
				t.Fatal("fixture did not exceed the idle budget")
			}
			got, err := os.ReadFile(filepath.Join(dst, "source"))
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("transfer content mismatch: %v", err)
			}
		})
	}
}

func TestRsyncParentDeadlineIsNotStall(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "rsync"), "#!/bin/sh\nexec sleep 60\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	err := rsync(ctx, SSHTarget{Host: "localhost", User: "runner", Port: "22"}, dir, "/work", nil, io.Discard, io.Discard, rsyncOptions{Timeout: time.Minute})
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "no I/O progress") {
		t.Fatalf("parent cancellation misclassified: %v", err)
	}
}
