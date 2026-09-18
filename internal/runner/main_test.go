package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type observedInput struct {
	*os.File
	read      chan struct{}
	remaining int
}

func (r *observedInput) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		select {
		case r.read <- struct{}{}:
		default:
		}
	}
	n, err := r.File.Read(p)
	r.remaining -= n
	return n, err
}

func TestMainOwnedInputCancellationAndEOF(t *testing.T) {
	var request bytes.Buffer
	if err := WriteRequest(&request, Request{BuildID: CurrentIdentity().BuildID, Operation: Collect, Workdir: t.TempDir()}, 0, nil); err != nil {
		t.Fatal(err)
	}
	for _, ending := range []string{"cancel", "eof", "cancel-at-eof"} {
		t.Run(ending, func(t *testing.T) {
			input, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			defer writer.Close()
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			cause := errors.New("ordinary request canceled")
			payload := request.Bytes()[:request.Len()/2]
			if ending == "cancel-at-eof" {
				payload = request.Bytes()
			}
			observed := &observedInput{File: input, read: make(chan struct{}, 1), remaining: len(payload)}
			var output bytes.Buffer
			done := make(chan int, 1)
			go func() { done <- Main(ctx, []string{"serve"}, observed, nopWriteCloser{&output}, io.Discard) }()
			// Send only the first half of a normal request frame, then stop input.
			if _, err := writer.Write(payload); err != nil {
				t.Fatal(err)
			}
			select {
			case <-observed.read:
			case <-time.After(5 * time.Second):
				t.Fatal("helper did not read input")
			}
			if ending != "eof" {
				cancel(cause)
			} else {
				_ = writer.Close()
			}
			select {
			case code := <-done:
				if code != 1 {
					t.Fatalf("incomplete request exit = %d", code)
				}
				if ending != "eof" && !strings.Contains(output.String(), cause.Error()) {
					t.Fatalf("response lost cause: %q", output.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("helper did not terminate")
			}
			if _, err := input.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("owned input remains open: %v", err)
			}
		})
	}
}

func TestCancellationInputPreservesEOFAndCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	r := cancellationInput{ctx: ctx, input: strings.NewReader("")}
	if _, err := r.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("EOF changed: %v", err)
	}
	cause := errors.New("caller canceled read")
	cancel(cause)
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, cause) {
		t.Fatalf("cause lost: %v", err)
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

type observedOutput struct {
	File    *os.File
	writing chan struct{}
}

func (w *observedOutput) Close() error { return w.File.Close() }

func (w *observedOutput) Write(data []byte) (int, error) {
	if len(data) >= 32<<10 {
		select {
		case w.writing <- struct{}{}:
		default:
		}
	}
	return w.File.Write(data)
}

func TestMainOwnedOutputCancellation(t *testing.T) {
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, "result.txt"), bytes.Repeat([]byte("a"), 1<<20), 0600); err != nil {
		t.Fatal(err)
	}
	var request bytes.Buffer
	if err := WriteRequest(&request, Request{BuildID: CurrentIdentity().BuildID, Operation: Collect, Workdir: workdir, Paths: []string{"result.txt"}}, 0, nil); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &observedOutput{File: writer, writing: make(chan struct{}, 1)}
	done := make(chan int, 1)
	go func() { done <- Main(ctx, []string{"serve"}, io.NopCloser(&request), output, io.Discard) }()
	// A normal result larger than the OS pipe capacity blocks without a consumer.
	select {
	case <-output.writing:
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not write result")
	}
	cancel()
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("canceled output exit = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not release output")
	}
	if _, err := writer.Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("owned output remains open: %v", err)
	}
}
