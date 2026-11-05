//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"testing"
)

func TestBrokenPipeWriterLatchesFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newBrokenPipeHandler(cancel)
	defer h.stop()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
	defer writer.Close()
	out := h.writer(writer)
	_, first := out.Write([]byte("workload output"))
	if !isBrokenPipe(first) || !h.broken.Load() || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("write = %v, broken = %t, context = %v", first, h.broken.Load(), ctx.Err())
	}
	writer.Close()
	// A fresh syscall now returns a closed-file error. Returning the original
	// error proves cleanup diagnostics do not write to the failed descriptor.
	if _, err := out.Write([]byte("cleanup diagnostic")); err != first {
		t.Fatalf("subsequent write = %v, want original error %v", err, first)
	}
}

func TestBrokenPipeHandlerPreservesRegularFiles(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	h := newBrokenPipeHandler(func() { t.Error("unexpected cancellation") }, file)
	defer h.stop()
	if h.writer(file) != file {
		t.Fatal("regular output file lost direct file inheritance")
	}
}

func TestBrokenPipeMonitor(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		for _, mode := range []string{"quiet", "direct-write", "inherited-child"} {
			t.Run(stream+"/"+mode, func(t *testing.T) {
				reader, writer, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				defer writer.Close()
				null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer null.Close()
				stdout, stderr := writer, null
				if stream == "stderr" {
					stdout, stderr = null, writer
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				h := newBrokenPipeHandler(cancel, stdout, stderr)
				defer h.stop()
				reader.Close()
				switch mode {
				case "direct-write":
					if _, err := writer.Write([]byte("unwrapped output")); !isBrokenPipe(err) {
						t.Fatalf("direct write = %v", err)
					}
				case "inherited-child":
					command := "printf child-output"
					if stream == "stderr" {
						command += " >&2"
					}
					child := exec.CommandContext(t.Context(), "sh", "-c", command)
					child.Stdout, child.Stderr = stdout, stderr
					if err := child.Run(); err == nil {
						t.Fatal("child wrote to broken stdio successfully")
					}
				}
				select {
				case <-ctx.Done():
				case <-time.After(3 * time.Second):
					t.Fatal("closed output did not cancel the operation")
				}
				if !h.broken.Load() {
					t.Fatal("closed output was not recorded")
				}
			})
		}
	}
}

func TestBrokenPipeMonitorIgnoresNonPipeOutputs(t *testing.T) {
	for _, kind := range []string{"regular-file", "terminal", "devnull"} {
		t.Run(kind, func(t *testing.T) {
			var file *os.File
			var err error
			switch kind {
			case "regular-file":
				file, err = os.CreateTemp(t.TempDir(), "output")
			case "devnull":
				file, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			case "terminal":
				var master *os.File
				master, file, err = pty.Open()
				if err == nil {
					master.Close()
				} // Even a hung-up terminal is not a broken pipe.
			}
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h := newBrokenPipeHandler(cancel, file, file)
			defer h.stop()
			select {
			case <-ctx.Done():
				t.Fatal("non-pipe output canceled the operation")
			case <-time.After(250 * time.Millisecond):
			}
			if h.broken.Load() {
				t.Fatal("non-pipe output was classified as broken")
			}
		})
	}
}

func TestBrokenPipeMonitorPreservesDescriptorFlags(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	raw, err := writer.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	flags := func() int {
		var value int
		var flagErr error
		if err := raw.Control(func(fd uintptr) { value, flagErr = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil {
			t.Fatal(err)
		}
		if flagErr != nil {
			t.Fatal(flagErr)
		}
		return value
	}
	before := flags()
	h := newBrokenPipeHandler(func() {}, writer)
	defer h.stop()
	if after := flags(); after != before {
		t.Fatalf("descriptor flags changed: before=%#x after=%#x", before, after)
	}
}
