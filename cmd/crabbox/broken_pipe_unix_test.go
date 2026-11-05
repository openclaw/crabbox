//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
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
	h := newBrokenPipeHandler(func() { t.Error("unexpected cancellation") })
	defer h.stop()
	if h.writer(file) != file {
		t.Fatal("regular output file lost direct file inheritance")
	}
}
