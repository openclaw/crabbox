package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type workspaceOwnerShortWriter struct {
	calls, failAt int
	err           error
}

func (w *workspaceOwnerShortWriter) Write(data []byte) (int, error) {
	w.calls++
	if w.calls == w.failAt {
		return len(data) - 1, w.err
	}
	return len(data), nil
}

func TestWorkspaceOwnerSetupDiagnosticPreservesWriteFailures(t *testing.T) {
	for _, test := range []struct {
		name, prefix, input string
		finishOnly          bool
	}{
		{name: "line-flush", input: "ordinary setup output\n"},
		{name: "buffer-flush", input: strings.Repeat("x", 160)},
		{name: "passthrough", prefix: strings.Repeat("x", 160), input: "more output"},
		{name: "passthrough-newline", prefix: strings.Repeat("x", 160), input: "more output\n"},
		{name: "trailing-partial", input: "ordinary partial output", finishOnly: true},
	} {
		for _, writerErr := range []error{nil, io.ErrClosedPipe} {
			t.Run(fmt.Sprintf("%s/error=%v", test.name, writerErr), func(t *testing.T) {
				destination := &workspaceOwnerShortWriter{failAt: 1, err: writerErr}
				_, stderr, finish := workspaceOwnerSetupStreams("test-marker", io.Discard, destination)
				if test.prefix != "" {
					destination.failAt = 2
					if _, err := io.WriteString(stderr, test.prefix); err != nil {
						t.Fatal(err)
					}
				}
				_, err := io.WriteString(stderr, test.input)
				if test.finishOnly && err != nil {
					t.Fatalf("partial line should be buffered: %v", err)
				}
				err = finish(err)
				want := writerErr
				if want == nil {
					want = io.ErrShortWrite
				}
				if !errors.Is(err, want) {
					t.Fatalf("forwarding error=%v, want %v", err, want)
				}
				if destination.calls != destination.failAt {
					t.Fatalf("failed output replayed: calls=%d", destination.calls)
				}
			})
		}
	}
}

func TestWorkspaceOwnerSetupFailureDoesNotRetrySSHPort(t *testing.T) {
	err := &workspaceOwnerSetupError{phase: "handoff", cause: exit(255, "connection closed")}
	if shouldRetrySSHPort(err) {
		t.Fatal("proven setup failure was retried as transport failure")
	}
}

func TestWorkspaceOwnerSetupDiagnosticFraming(t *testing.T) {
	marker := workspaceOwnerSetupPrefix + strings.Repeat("1", 32)
	failure := marker + " failed registration\n"
	started := marker + " started\n"
	for _, test := range []struct {
		name, input, output, phase string
	}{
		{"setup", failure, "", "registration"},
		{"first-failure", failure + marker + " failed handoff\n", "", "registration"},
		{"workload", started + failure, failure, ""},
		{"other-invocation", workspaceOwnerSetupPrefix + "other failed registration\n", workspaceOwnerSetupPrefix + "other failed registration\n", ""},
		{"partial", strings.TrimSuffix(failure, "\n"), strings.TrimSuffix(failure, "\n"), ""},
		{"embedded", "warning: " + failure, "warning: " + failure, ""},
		{"unknown-phase", marker + " failed arbitrary-output\n", marker + " failed arbitrary-output\n", ""},
		{"bounded", strings.Repeat("x", 1024*1024) + "\n" + failure, strings.Repeat("x", 1024*1024) + "\n", "registration"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, chunkSize := range []int{1, 17, 4096} {
				var out bytes.Buffer
				writer := workspaceOwnerSetupWriter{destination: &out, marker: marker}
				data := []byte(test.input)
				for len(data) > 0 {
					n := min(chunkSize, len(data))
					if count, err := writer.Write(data[:n]); err != nil || count != n {
						t.Fatalf("write count=%d err=%v", count, err)
					}
					if len(writer.pending) >= 160 {
						t.Fatal("diagnostic parser exceeded its buffer bound")
					}
					data = data[n:]
				}
				if err := writer.flush(); err != nil {
					t.Fatal(err)
				}
				if out.String() != test.output || writer.failure != test.phase {
					t.Fatalf("chunk=%d output-bytes=%d phase=%q", chunkSize, out.Len(), writer.failure)
				}
			}
		})
	}
}
